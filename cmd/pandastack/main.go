// pandastack is the user-facing CLI for the PandaStack microVM sandbox platform.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

const (
	defaultAPI = "https://api.pandastack.ai"
	version    = "0.1.0"
)

// Supabase defaults are var (not const) so the managed-cloud build can
// inject project-specific values via `-ldflags "-X main.defaultSupabaseURL=...
// -X main.defaultSupabaseKey=..."`. The OSS build ships empty; users must
// set PANDASTACK_SUPABASE_URL and PANDASTACK_SUPABASE_ANON_KEY (or use a
// local Supabase env file) when running `pandastack auth` in supabase mode.
var (
	defaultSupabaseURL = ""
	defaultSupabaseKey = ""
)

var outputJSON bool

type configFile struct {
	APIURL    string `json:"api_url"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func main() {
	args, err := parseGlobalFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	cmd := args[0]
	args = args[1:]
	switch cmd {
	case "auth":
		err = authCmd(args)
	case "template", "templates":
		err = templateCmd(args)
	case "sandbox", "sandboxes", "sb":
		err = sandboxCmd(args)
	case "token", "tokens":
		err = tokenCmd(args)
	case "function", "functions", "fn":
		err = functionCmd(args)
	case "schedule", "schedules":
		err = scheduleCmd(args)
	case "ssh":
		// shorthand: pandastack ssh <id>  (same as pandastack sandbox ssh <id>)
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: pandastack ssh <sandbox-id>")
			os.Exit(2)
		}
		err = sandboxSSH(args[0])
	case "version", "-v", "--version":
		err = versionCmd()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func parseGlobalFlags(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		switch {
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				return nil, errors.New("-o requires a value (use -o json)")
			}
			i++
			if args[i] != "json" {
				return nil, fmt.Errorf("unsupported output %q (only json is supported)", args[i])
			}
			outputJSON = true
		case strings.HasPrefix(a, "-o="):
			if strings.TrimPrefix(a, "-o=") != "json" {
				return nil, fmt.Errorf("unsupported output %q (only json is supported)", strings.TrimPrefix(a, "-o="))
			}
			outputJSON = true
		case strings.HasPrefix(a, "--output="):
			if strings.TrimPrefix(a, "--output=") != "json" {
				return nil, fmt.Errorf("unsupported output %q (only json is supported)", strings.TrimPrefix(a, "--output="))
			}
			outputJSON = true
		default:
			out = append(out, a)
		}
	}
	return out, nil
}

func usage() {
	fmt.Println(`pandastack — microVM sandboxes for AI agents.

USAGE
  pandastack [-o json] <command> [subcommand] [flags]

COMMANDS
  auth login                                      log in and save a CLI token
  auth logout                                     remove saved CLI credentials
  auth whoami                                     show the current user

  template list                                   list available templates
  template build -f FILE -n NAME [--size-mb N] [--cpu N] [--memory-mb N] [--replace]
                                                  bake a snapshot from a Dockerfile
  template delete NAME                            remove a template

  sandbox create --template NAME [--ttl 1h]       create a sandbox
  sandbox list                                    list your sandboxes
  sandbox get ID                                  show sandbox details
  sandbox exec ID [--timeout N] -- CMD [ARGS...]  run a command in a sandbox
  sandbox logs ID [--no-follow] [--stream STREAM] stream sandbox logs
  sandbox ssh ID                                  open an interactive SSH shell (works from anywhere)
  sandbox cp SRC DST                              copy a file to/from a sandbox
  sandbox pause ID                                pause a sandbox
  sandbox resume ID                               resume a sandbox
  sandbox snapshot ID                             create a sandbox snapshot
  sandbox fork ID                                 fork a running sandbox
  sandbox hibernate ID                            hibernate a sandbox (memory snapshot)
  sandbox wake ID                                 wake a hibernated sandbox
  sandbox set-ttl ID DURATION                     update sandbox TTL (e.g. 1h)
  sandbox preview-url ID PORT [--ttl 1h]          mint a public preview URL
  sandbox delete ID                               delete a sandbox

  token list                                      list your API tokens
  token create NAME                               create a new API token
  token revoke PREFIX                             revoke a token by prefix

  function list                                   list deployed functions
  function deploy FILE [--name NAME] [--runtime python|nodejs] [--env KEY=VAL] [--public] [--template NAME]
                                                   deploy a function from source code
  function get ID                                 show function details as JSON
  function update ID [--name NAME] [--env KEY=VAL] [--public[=true|false]]
                                                   update a function
  function delete ID                              delete a function
  function logs ID                                list recent function runs
  function run ID                                 trigger a function manually

  schedule list                                   list all schedules
  schedule create --name NAME --function-id ID --cron SPEC
                                                   create a cron schedule
  schedule get ID                                 show schedule details as JSON
  schedule pause ID                               pause a schedule
  schedule resume ID                              resume a schedule
  schedule trigger ID                             trigger a schedule immediately
  schedule delete ID                              delete a schedule

  ssh ID                                          shorthand for sandbox ssh ID

  version                                         print client and server versions
  help                                            show this help

GLOBAL FLAGS
  -o json              print JSON for list/get-style commands

ENV
  PANDASTACK_API       base URL (default: https://api.pandastack.ai)
  PANDASTACK_TOKEN     bearer token (overrides saved config)
  PANDASTACK_SUPABASE_URL / PANDASTACK_SUPABASE_ANON_KEY for auth login`)
}

// ----------------------------------------------------------------------------
// Subcommand: auth
// ----------------------------------------------------------------------------

func authCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("auth requires a subcommand: login | logout | whoami")
	}
	switch args[0] {
	case "login":
		return authLogin()
	case "logout":
		return authLogout()
	case "whoami":
		return authWhoami()
	default:
		return fmt.Errorf("unknown auth subcommand: %s", args[0])
	}
}

func authLogin() error {
	supabaseURL, supabaseKey := supabaseConfig()
	if supabaseURL == "" || supabaseKey == "" {
		fmt.Println("Supabase auth is not configured.")
		fmt.Println("Get a token from https://app.pandastack.ai/settings/tokens and run: export PANDASTACK_TOKEN=cfat_...")
		return nil
	}
	email, err := promptLine("Email: ")
	if err != nil {
		return err
	}
	password, err := promptPassword("Password: ")
	if err != nil {
		return err
	}
	jwt, expiresAt, err := supabasePasswordLogin(supabaseURL, supabaseKey, email, password)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	label := "cli-" + host
	tok, err := createAccountToken(jwt, label)
	if err != nil {
		return err
	}
	cfg := configFile{APIURL: apiBase(), Token: tok, ExpiresAt: expiresAt}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Logged in. Saved credentials to %s\n", configPath())
	return nil
}

func authLogout() error {
	p := configPath()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("Logged out. Removed %s\n", p)
	return nil
}

func authWhoami() error {
	body, err := apiGET("/v1/me")
	if err != nil {
		return fmt.Errorf("whoami failed: %w", err)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(v)
	}
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	for _, k := range []string{"id", "email", "name", "workspace", "auth_method"} {
		if val, ok := m[k]; ok {
			fmt.Printf("%s: %v\n", k, val)
		}
	}
	return nil
}

func supabaseConfig() (string, string) {
	u := strings.TrimRight(os.Getenv("PANDASTACK_SUPABASE_URL"), "/")
	k := os.Getenv("PANDASTACK_SUPABASE_ANON_KEY")
	if u != "" && k != "" {
		return u, k
	}
	if fu, fk := readSupabaseEnvFile(); fu != "" || fk != "" {
		if u == "" {
			u = fu
		}
		if k == "" {
			k = fk
		}
	}
	if u == "" {
		u = defaultSupabaseURL
	}
	if k == "" {
		k = defaultSupabaseKey
	}
	return strings.TrimRight(u, "/"), k
}

func readSupabaseEnvFile() (string, string) {
	wd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	for {
		p := filepath.Join(wd, "dashboard", ".env.production")
		b, err := os.ReadFile(p)
		if err == nil {
			var u, k string
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "NEXT_PUBLIC_SUPABASE_URL=") {
					u = strings.TrimSpace(strings.TrimPrefix(line, "NEXT_PUBLIC_SUPABASE_URL="))
				}
				if strings.HasPrefix(line, "NEXT_PUBLIC_SUPABASE_ANON_KEY=") {
					k = strings.TrimSpace(strings.TrimPrefix(line, "NEXT_PUBLIC_SUPABASE_ANON_KEY="))
				}
			}
			return u, k
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", ""
		}
		wd = parent
	}
}

func supabasePasswordLogin(baseURL, anonKey, email, password string) (string, string, error) {
	reqBody := map[string]string{"email": strings.TrimSpace(email), "password": password}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", baseURL+"/auth/v1/token?grant_type=password", bytes.NewReader(b))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", anonKey)
	req.Header.Set("Authorization", "Bearer "+anonKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("Supabase login failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var out struct {
		AccessToken string  `json:"access_token"`
		ExpiresAt   float64 `json:"expires_at"`
		ExpiresIn   int64   `json:"expires_in"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", "", fmt.Errorf("decode Supabase response: %w", err)
	}
	if out.AccessToken == "" {
		return "", "", errors.New("Supabase login did not return an access token")
	}
	expires := ""
	if out.ExpiresAt > 0 {
		expires = time.Unix(int64(out.ExpiresAt), 0).UTC().Format(time.RFC3339)
	} else if out.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return out.AccessToken, expires, nil
}

func createAccountToken(jwt, label string) (string, error) {
	body := map[string]string{"name": label, "label": label}
	rb, err := apiDoWithToken("POST", "/v1/me/tokens", body, jwt)
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("token response did not include a token")
	}
	return out.Token, nil
}

// ----------------------------------------------------------------------------
// Subcommand: template
// ----------------------------------------------------------------------------

func templateCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("template requires a subcommand: list | build | delete")
	}
	switch args[0] {
	case "list", "ls":
		return templateList()
	case "build":
		return templateBuild(args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			return errors.New("template delete NAME")
		}
		return templateDelete(args[1])
	default:
		return fmt.Errorf("unknown template subcommand: %s", args[0])
	}
}

func templateList() error {
	body, err := apiGET("/v1/templates")
	if err != nil {
		return err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("no templates")
		return nil
	}
	fmt.Printf("%-24s %12s  %s\n", "NAME", "SIZE", "BUILT")
	for _, t := range list {
		name, _ := t["name"].(string)
		bytes, _ := t["size_bytes"].(float64)
		built := ""
		if m, ok := t["meta"].(map[string]any); ok {
			if s, ok := m["built_at"].(string); ok {
				built = s
			}
		}
		fmt.Printf("%-24s %12s  %s\n", name, humanBytes(int64(bytes)), built)
	}
	return nil
}

func templateDelete(name string) error {
	if err := apiDELETE("/v1/templates/" + url.PathEscape(name)); err != nil {
		return err
	}
	fmt.Printf("deleted template %q\n", name)
	return nil
}

func templateBuild(args []string) error {
	fs := flag.NewFlagSet("template build", flag.ExitOnError)
	file := fs.String("f", "Dockerfile", "Dockerfile path")
	name := fs.String("n", "", "template name (required)")
	context := fs.String("context", ".", "Docker build context directory")
	sizeMB := fs.Int("size-mb", 2048, "Target ext4 image size (MB)")
	kernel := fs.String("kernel", "vmlinux-5.10", "Kernel image to associate with the template")
	cpu := fs.Int("cpu", 1, "vCPU count baked into the template")
	mem := fs.Int("memory-mb", 512, "Memory (MiB) baked into the template")
	replace := fs.Bool("replace", false, "Allow overwriting an existing template with the same name")
	keepImage := fs.Bool("keep-image", false, "Don't remove the intermediate Docker image after build")
	_ = fs.Parse(args)

	if *name == "" {
		return errors.New("--name (-n) is required")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found on PATH (required for `template build`)")
	}
	dockerfilePath, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dockerfilePath); err != nil {
		return fmt.Errorf("dockerfile not found: %s", dockerfilePath)
	}

	tag := fmt.Sprintf("pandastack-tpl-%s:%d", *name, time.Now().UnixNano())
	fmt.Printf("→ docker build  -t %s -f %s %s\n", tag, *file, *context)
	if err := runStream("docker", "build", "-t", tag, "-f", dockerfilePath, *context); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}

	containerName := fmt.Sprintf("pandastack-tpl-%s-%d", *name, time.Now().UnixNano())
	fmt.Printf("→ docker create %s\n", tag)
	if err := runStream("docker", "create", "--name", containerName, tag, "/bin/true"); err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		if !*keepImage {
			_ = exec.Command("docker", "rmi", "-f", tag).Run()
		}
	}()

	tmpTar := fmt.Sprintf(".pandastack-rootfs-%d.tar", time.Now().UnixNano())
	f, err := os.OpenFile(tmpTar, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	defer os.Remove(tmpTar)

	fmt.Printf("→ docker export → %s\n", tmpTar)
	if err := runShell(fmt.Sprintf("docker export %s > %s", shellQuote(containerName), shellQuote(tmpTar))); err != nil {
		return fmt.Errorf("docker export: %w", err)
	}
	st, _ := os.Stat(tmpTar)
	fmt.Printf("  rootfs tar size: %s\n", humanBytes(st.Size()))

	fmt.Printf("→ uploading to %s/v1/templates/build (name=%s, size_mb=%d, cpu=%d, memory_mb=%d)\n", apiBase(), *name, *sizeMB, *cpu, *mem)
	build, err := uploadBuild(*name, *sizeMB, *kernel, tmpTar, *cpu, *mem, *replace)
	if err != nil {
		return err
	}
	id, _ := build["id"].(string)
	fmt.Printf("  build id: %s · status: %s\n", id, build["status"])

	last := ""
	for {
		body, err := apiGET("/v1/templates/builds/" + url.PathEscape(id))
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		var b map[string]any
		if err := json.Unmarshal(body, &b); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		status, _ := b["status"].(string)
		if status != last {
			fmt.Printf("  status: %s\n", status)
			last = status
		}
		switch status {
		case "done":
			fmt.Printf("✓ template %q baked successfully\n", *name)
			return nil
		case "failed":
			return fmt.Errorf("build failed: %v", b["error"])
		}
		time.Sleep(2 * time.Second)
	}
}

func uploadBuild(name string, sizeMB int, kernel, tarPath string, cpu, memMB int, replace bool) (map[string]any, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		defer mw.Close()
		_ = mw.WriteField("name", name)
		_ = mw.WriteField("size_mb", fmt.Sprintf("%d", sizeMB))
		_ = mw.WriteField("kernel", kernel)
		_ = mw.WriteField("cpu", fmt.Sprintf("%d", cpu))
		_ = mw.WriteField("memory_mb", fmt.Sprintf("%d", memMB))
		fw, err := mw.CreateFormFile("rootfs", filepath.Base(tarPath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(fw, f); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	uploadURL := apiBase() + "/v1/templates/build"
	if replace {
		uploadURL += "?replace=true"
	}
	req, err := http.NewRequest("POST", uploadURL, pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addAuth(req)
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, httpError(resp.StatusCode, "POST", strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode: %w (%s)", err, string(body))
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// Subcommand: sandbox
// ----------------------------------------------------------------------------

func sandboxCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("sandbox requires a subcommand: create | list | get | exec | logs | ssh | cp | pause | resume | snapshot | fork | hibernate | wake | set-ttl | preview-url | delete")
	}
	switch args[0] {
	case "create":
		return sandboxCreate(args[1:])
	case "list", "ls":
		return sandboxList()
	case "get", "inspect":
		if len(args) < 2 {
			return errors.New("sandbox get ID")
		}
		return sandboxGet(args[1])
	case "exec":
		return sandboxExec(args[1:])
	case "logs":
		return sandboxLogs(args[1:])
	case "ssh":
		if len(args) < 2 {
			return errors.New("sandbox ssh ID")
		}
		return sandboxSSH(args[1])
	case "cp":
		if len(args) < 3 {
			return errors.New("sandbox cp SRC DST")
		}
		return sandboxCP(args[1], args[2])
	case "pause":
		if len(args) < 2 {
			return errors.New("sandbox pause ID")
		}
		return sandboxAction(args[1], "pause")
	case "resume":
		if len(args) < 2 {
			return errors.New("sandbox resume ID")
		}
		return sandboxAction(args[1], "resume")
	case "snapshot":
		if len(args) < 2 {
			return errors.New("sandbox snapshot ID")
		}
		return sandboxSnapshot(args[1])
	case "fork":
		if len(args) < 2 {
			return errors.New("sandbox fork ID")
		}
		return sandboxFork(args[1])
	case "hibernate":
		if len(args) < 2 {
			return errors.New("sandbox hibernate ID")
		}
		return sandboxAction(args[1], "hibernate")
	case "wake":
		if len(args) < 2 {
			return errors.New("sandbox wake ID")
		}
		return sandboxAction(args[1], "wake")
	case "set-ttl":
		if len(args) < 3 {
			return errors.New("sandbox set-ttl ID DURATION (e.g. 1h)")
		}
		return sandboxSetTTL(args[1], args[2])
	case "preview-url":
		return sandboxPreviewURL(args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			return errors.New("sandbox delete ID")
		}
		return sandboxDelete(args[1])
	default:
		return fmt.Errorf("unknown sandbox subcommand: %s", args[0])
	}
}

func sandboxCreate(args []string) error {
	fs := flag.NewFlagSet("sandbox create", flag.ExitOnError)
	tpl := fs.String("template", "", "template name (required)")
	ttl := fs.String("ttl", "", "lifetime (e.g. 1h, 30m, 24h)")
	cpu := fs.Int("cpu", 0, "(deprecated, ignored) vCPU is baked into the template")
	mem := fs.Int("memory-mb", 0, "(deprecated, ignored) memory is baked into the template")
	_ = fs.Parse(args)

	if *tpl == "" {
		return errors.New("--template is required")
	}
	if *cpu > 0 || *mem > 0 {
		fmt.Fprintln(os.Stderr, "warning: --cpu/--memory-mb are deprecated and ignored — CPU and memory are baked into the template. Use `pandastack template build --cpu --memory-mb` to bake a custom size.")
	}
	req := map[string]any{"template": *tpl}
	if *ttl != "" {
		d, err := time.ParseDuration(*ttl)
		if err != nil {
			return fmt.Errorf("invalid --ttl: %w", err)
		}
		req["ttl_seconds"] = int(d.Seconds())
	}
	body, err := apiPOST("/v1/sandboxes", req)
	if err != nil {
		return err
	}
	var sb map[string]any
	_ = json.Unmarshal(body, &sb)
	if outputJSON {
		return printJSON(sb)
	}
	fmt.Printf("✓ sandbox %s  template=%s  boot=%vms  ip=%s\n",
		sb["id"], sb["template"], sb["boot_ms"], sb["guest_ip"])
	return nil
}

func sandboxList() error {
	body, err := apiGET("/v1/sandboxes")
	if err != nil {
		return err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("no sandboxes")
		return nil
	}
	fmt.Printf("%-38s %-20s %-10s %s\n", "ID", "TEMPLATE", "STATUS", "BOOT_MS")
	for _, s := range list {
		fmt.Printf("%-38s %-20s %-10s %v\n", s["id"], s["template"], s["status"], s["boot_ms"])
	}
	return nil
}

func sandboxGet(id string) error {
	body, err := apiGET("/v1/sandboxes/" + url.PathEscape(id))
	if err != nil {
		return err
	}
	var sb map[string]any
	if err := json.Unmarshal(body, &sb); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(sb)
	}
	for _, k := range []string{"id", "template", "status", "guest_ip", "cpu", "memory_mb", "boot_ms", "created_at"} {
		if v, ok := sb[k]; ok {
			fmt.Printf("%s: %v\n", k, v)
		}
	}
	return nil
}

func sandboxExec(args []string) error {
	if len(args) < 1 {
		return errors.New("sandbox exec ID [--timeout N] -- CMD [ARGS...]")
	}
	id := args[0]
	fs := flag.NewFlagSet("sandbox exec", flag.ExitOnError)
	timeout := fs.Int("timeout", 0, "timeout in seconds")
	_ = fs.Parse(args[1:])
	cmdArgs := fs.Args()
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if len(cmdArgs) == 0 {
		return errors.New("missing command (use: pandastack sandbox exec ID -- CMD [ARGS...])")
	}
	req := map[string]any{"cmd": joinShell(cmdArgs)}
	if *timeout > 0 {
		req["timeout_seconds"] = *timeout
	}
	body, err := apiPOST("/v1/sandboxes/"+url.PathEscape(id)+"/exec", req)
	if err != nil {
		return err
	}
	var res struct {
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		ExitCode   int    `json:"exit_code"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Fprint(os.Stdout, res.Stdout)
	fmt.Fprint(os.Stderr, res.Stderr)
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

func sandboxLogs(args []string) error {
	if len(args) < 1 {
		return errors.New("sandbox logs ID [--no-follow] [--stream stdout|stderr|both]")
	}
	id := args[0]
	fs := flag.NewFlagSet("sandbox logs", flag.ExitOnError)
	noFollow := fs.Bool("no-follow", false, "do not follow logs")
	stream := fs.String("stream", "both", "stdout, stderr, or both")
	_ = fs.Parse(args[1:])
	if *stream != "stdout" && *stream != "stderr" && *stream != "both" {
		return errors.New("--stream must be stdout, stderr, or both")
	}
	q := url.Values{}
	q.Set("follow", strconv.FormatBool(!*noFollow))
	if *stream != "both" {
		q.Set("stream", *stream)
	}
	return streamAPI("/v1/sandboxes/"+url.PathEscape(id)+"/logs?"+q.Encode(), !*noFollow)
}

func sandboxSSH(id string) error {
	// Verify sandbox exists.
	body, err := apiGET("/v1/sandboxes/" + url.PathEscape(id))
	if err != nil {
		return err
	}
	var sb map[string]any
	if err := json.Unmarshal(body, &sb); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	// Get initial terminal dimensions.
	rows, cols := 24, 80
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			cols, rows = w, h
		}
	}

	// Build WebSocket URL: https → wss, http → ws.
	base := apiBase()
	wsBase := strings.NewReplacer("https://", "wss://", "http://", "ws://").Replace(base)
	wsURL := fmt.Sprintf("%s/v1/sandboxes/%s/ssh?rows=%d&cols=%d",
		wsBase, url.PathEscape(id), rows, cols)

	hdr := http.Header{}
	if tok := authToken(); tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		return fmt.Errorf("connect to sandbox: %w", err)
	}
	defer conn.Close()

	// Switch terminal to raw mode so control characters pass through unmodified.
	var oldState *term.State
	if term.IsTerminal(fd) {
		oldState, err = term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("raw terminal: %w", err)
		}
		defer term.Restore(fd, oldState)
	}

	var writeMu sync.Mutex
	sendBin := func(b []byte) {
		writeMu.Lock()
		_ = conn.WriteMessage(websocket.BinaryMessage, b)
		writeMu.Unlock()
	}
	sendText := func(v any) {
		b, _ := json.Marshal(v)
		writeMu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, b)
		writeMu.Unlock()
	}

	exitCode := 0
	done := make(chan struct{})

	// WS → stdout (+ handle {"exit":N} and {"error":"..."} text frames)
	go func() {
		defer close(done)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				_, _ = os.Stdout.Write(data)
			case websocket.TextMessage:
				var ctrl struct {
					Exit  *int   `json:"exit,omitempty"`
					Error string `json:"error,omitempty"`
				}
				if json.Unmarshal(data, &ctrl) == nil {
					if ctrl.Exit != nil {
						exitCode = *ctrl.Exit
						return
					}
					if ctrl.Error != "" {
						fmt.Fprintln(os.Stderr, "\r\nerror:", ctrl.Error)
						exitCode = 1
						return
					}
				}
			}
		}
	}()

	// stdin → WS binary frames
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				sendBin(buf[:n])
			}
			if err != nil {
				conn.Close()
				return
			}
		}
	}()

	// SIGWINCH → resize JSON
	if term.IsTerminal(fd) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGWINCH)
		go func() {
			for range sigCh {
				if w, h, err := term.GetSize(fd); err == nil {
					sendText(map[string]any{
						"resize": map[string]int{"rows": h, "cols": w},
					})
				}
			}
		}()
		defer signal.Stop(sigCh)
	}

	<-done

	// Restore terminal before any final output.
	if oldState != nil {
		term.Restore(fd, oldState)
		oldState = nil
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func sandboxCP(src, dst string) error {
	srcID, srcPath, srcRemote := splitRemote(src)
	dstID, dstPath, dstRemote := splitRemote(dst)
	switch {
	case srcRemote && !dstRemote:
		body, err := apiGET("/v1/sandboxes/" + url.PathEscape(srcID) + "/fs?path=" + url.QueryEscape(srcPath))
		if err != nil {
			return err
		}
		out := dst
		if st, err := os.Stat(dst); err == nil && st.IsDir() {
			out = filepath.Join(dst, filepath.Base(srcPath))
		}
		return os.WriteFile(out, body, 0o644)
	case !srcRemote && dstRemote:
		body, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		_, err = apiPUTRaw("/v1/sandboxes/"+url.PathEscape(dstID)+"/fs?path="+url.QueryEscape(dstPath), body)
		if err == nil {
			fmt.Printf("uploaded %s to %s:%s\n", src, dstID, dstPath)
		}
		return err
	case srcRemote && dstRemote:
		return errors.New("copying directly between two sandboxes is not supported")
	default:
		return errors.New("one side must be a sandbox path like <sandbox-id>:/path")
	}
}

func sandboxAction(id, action string) error {
	_, err := apiPOST("/v1/sandboxes/"+url.PathEscape(id)+"/"+action, map[string]any{})
	if err != nil {
		return err
	}
	past := map[string]string{"pause": "paused", "resume": "resumed"}[action]
	if past == "" {
		past = action + "ed"
	}
	fmt.Printf("%s sandbox %s\n", past, id)
	return nil
}

func sandboxSnapshot(id string) error {
	body, err := apiPOST("/v1/sandboxes/"+url.PathEscape(id)+"/snapshots", map[string]any{})
	if err != nil {
		return err
	}
	var snap any
	_ = json.Unmarshal(body, &snap)
	if outputJSON {
		return printJSON(snap)
	}
	fmt.Printf("created snapshot for sandbox %s\n", id)
	return nil
}

func sandboxDelete(id string) error {
	if err := apiDELETE("/v1/sandboxes/" + url.PathEscape(id)); err != nil {
		return err
	}
	fmt.Printf("deleted sandbox %s\n", id)
	return nil
}

// ----------------------------------------------------------------------------
// Version
// ----------------------------------------------------------------------------

func versionCmd() error {
	client := map[string]string{"client": "pandastack", "version": version}
	body, err := apiGET("/v1/version")
	if err != nil {
		if outputJSON {
			return printJSON(map[string]any{"client": client, "server_error": err.Error()})
		}
		fmt.Println("pandastack client v" + version)
		fmt.Println("server: unavailable (" + err.Error() + ")")
		return nil
	}
	var server map[string]any
	_ = json.Unmarshal(body, &server)
	if outputJSON {
		return printJSON(map[string]any{"client": client, "server": server, "outdated": versionMismatch(version, fmt.Sprint(server["semver"]))})
	}
	fmt.Println("pandastack client v" + version)
	fmt.Printf("server %s %s commit=%v built=%v\n", server["service"], server["semver"], server["commit"], server["build_time"])
	if versionMismatch(version, fmt.Sprint(server["semver"])) {
		fmt.Println("hint: client and server major.minor versions differ; consider upgrading pandastack")
	}
	return nil
}

// ----------------------------------------------------------------------------
// HTTP helpers
// ----------------------------------------------------------------------------

func apiBase() string {
	if v := os.Getenv("PANDASTACK_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if cfg, err := readConfig(); err == nil && cfg.APIURL != "" {
		return strings.TrimRight(cfg.APIURL, "/")
	}
	return defaultAPI
}

func authToken() string {
	if tok := os.Getenv("PANDASTACK_TOKEN"); tok != "" {
		return tok
	}
	cfg, err := readConfig()
	if err != nil {
		return ""
	}
	return cfg.Token
}

func addAuth(req *http.Request) {
	if tok := authToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

func apiGET(path string) ([]byte, error)            { return apiDo("GET", path, nil) }
func apiDELETE(path string) error                   { _, err := apiDo("DELETE", path, nil); return err }
func apiPOST(path string, body any) ([]byte, error) { return apiDo("POST", path, body) }
func apiPUTRaw(path string, body []byte) ([]byte, error) {
	return apiDoRaw("PUT", path, bytes.NewReader(body), "application/octet-stream", authToken(), 60*time.Second)
}
func apiDo(method, path string, body any) ([]byte, error) {
	return apiDoWithToken(method, path, body, authToken())
}
func apiDoWithToken(method, path string, body any, token string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	return apiDoRaw(method, path, rd, "application/json", token, 60*time.Second)
}

func apiDoRaw(method, path string, rd io.Reader, contentType, token string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(method, apiBase()+path, rd)
	if err != nil {
		return nil, err
	}
	if rd != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, httpError(resp.StatusCode, method, strings.TrimSpace(string(rb)))
	}
	return rb, nil
}

func httpError(status int, method, body string) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("Not logged in. Run `pandastack auth login` or set PANDASTACK_TOKEN")
	}
	if body == "" {
		return fmt.Errorf("HTTP %d %s", status, method)
	}
	return fmt.Errorf("HTTP %d %s: %s", status, method, body)
}

func streamAPI(path string, sse bool) error {
	req, err := http.NewRequest("GET", apiBase()+path, nil)
	if err != nil {
		return err
	}
	addAuth(req)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return httpError(resp.StatusCode, "GET", strings.TrimSpace(string(b)))
	}
	if !sse {
		_, err = io.Copy(os.Stdout, resp.Body)
		return err
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var payload map[string]any
		if json.Unmarshal([]byte(data), &payload) == nil {
			if s, ok := payload["line"].(string); ok {
				fmt.Print(s)
			} else if s, ok := payload["chunk"].(string); ok {
				fmt.Print(s)
			}
		} else if data != "" {
			fmt.Println(data)
		}
	}
	return scanner.Err()
}

// ----------------------------------------------------------------------------
// Config and utilities
// ----------------------------------------------------------------------------

func configPath() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "pandastack", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "pandastack", "config.json")
}

func readConfig() (configFile, error) {
	var cfg configFile
	b, err := os.ReadFile(configPath())
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(b, &cfg)
}

func saveConfig(cfg configFile) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	s, err := r.ReadString('\n')
	return strings.TrimSpace(s), err
}

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if _, err := exec.LookPath("stty"); err == nil {
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		_ = off.Run()
		defer func() {
			on := exec.Command("stty", "echo")
			on.Stdin = os.Stdin
			_ = on.Run()
		}()
		defer fmt.Fprintln(os.Stderr)
	}
	r := bufio.NewReader(os.Stdin)
	s, err := r.ReadString('\n')
	return strings.TrimRight(s, "\r\n"), err
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runStream(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runShell(line string) error {
	c := exec.Command("/bin/sh", "-c", line)
	c.Stderr = os.Stderr
	return c.Run()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func joinShell(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func splitRemote(s string) (id, path string, ok bool) {
	idx := strings.Index(s, ":")
	if idx <= 0 {
		return "", "", false
	}
	id, path = s[:idx], s[idx+1:]
	if id == "" || path == "" {
		return "", "", false
	}
	return id, path, true
}

func isLikelyPrivate(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(u), 0
	for n2 := n / u; n2 >= u; n2 /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func versionMismatch(client, server string) bool {
	c := majorMinor(client)
	s := majorMinor(server)
	return c != "" && s != "" && c != s
}

func majorMinor(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// ----------------------------------------------------------------------------
// Subcommand: sandbox fork / set-ttl / preview-url
// ----------------------------------------------------------------------------

func sandboxFork(id string) error {
	body, err := apiPOST("/v1/sandboxes/"+url.PathEscape(id)+"/fork", map[string]any{"count": 1, "mode": "warm"})
	if err != nil {
		return err
	}
	var resp any
	_ = json.Unmarshal(body, &resp)
	if outputJSON {
		return printJSON(resp)
	}
	if m, ok := resp.(map[string]any); ok {
		if children, ok := m["children"].([]any); ok && len(children) > 0 {
			if first, ok := children[0].(map[string]any); ok {
				fmt.Printf("✓ forked sandbox %s -> %v (boot=%vms)\n", id, first["id"], first["boot_ms"])
				return nil
			}
		}
	}
	fmt.Printf("✓ forked sandbox %s\n", id)
	return nil
}

func sandboxSetTTL(id, dur string) error {
	d, err := time.ParseDuration(dur)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", dur, err)
	}
	body, err := apiDo("PATCH", "/v1/sandboxes/"+url.PathEscape(id)+"/lifecycle", map[string]any{"ttl_seconds": int(d.Seconds())})
	if err != nil {
		return err
	}
	var resp any
	_ = json.Unmarshal(body, &resp)
	if outputJSON {
		return printJSON(resp)
	}
	fmt.Printf("✓ ttl set to %s on sandbox %s\n", d, id)
	return nil
}

func sandboxPreviewURL(args []string) error {
	fs := flag.NewFlagSet("sandbox preview-url", flag.ExitOnError)
	ttl := fs.String("ttl", "1h", "URL lifetime (e.g. 1h, 24h; max 7d)")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("sandbox preview-url ID PORT [--ttl 1h]")
	}
	id := rest[0]
	port, err := strconv.Atoi(rest[1])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid PORT %q", rest[1])
	}
	d, err := time.ParseDuration(*ttl)
	if err != nil {
		return fmt.Errorf("invalid --ttl: %w", err)
	}
	body, err := apiPOST("/v1/sandboxes/"+url.PathEscape(id)+"/preview-token", map[string]any{
		"port":        port,
		"ttl_seconds": int(d.Seconds()),
	})
	if err != nil {
		return err
	}
	var resp map[string]any
	_ = json.Unmarshal(body, &resp)
	if outputJSON {
		return printJSON(resp)
	}
	if u, ok := resp["url"].(string); ok {
		fmt.Println(u)
		if exp, ok := resp["expires_at"].(string); ok {
			fmt.Fprintf(os.Stderr, "expires_at=%s\n", exp)
		}
		return nil
	}
	return printJSON(resp)
}

// ----------------------------------------------------------------------------
// Subcommand: token
// ----------------------------------------------------------------------------

func tokenCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("token requires a subcommand: list | create | revoke")
	}
	switch args[0] {
	case "list", "ls":
		return tokenList()
	case "create", "new":
		if len(args) < 2 {
			return errors.New("token create NAME")
		}
		return tokenCreate(args[1])
	case "revoke", "delete", "rm":
		if len(args) < 2 {
			return errors.New("token revoke PREFIX")
		}
		return tokenRevoke(args[1])
	default:
		return fmt.Errorf("unknown token subcommand: %s", args[0])
	}
}

func tokenList() error {
	body, err := apiGET("/v1/me/tokens")
	if err != nil {
		return err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("no tokens")
		return nil
	}
	fmt.Printf("%-16s %-32s %s\n", "PREFIX", "NAME", "CREATED")
	for _, t := range list {
		fmt.Printf("%-16s %-32s %v\n", t["prefix"], t["name"], t["created_at"])
	}
	return nil
}

func tokenCreate(name string) error {
	body, err := apiPOST("/v1/me/tokens", map[string]any{"name": name})
	if err != nil {
		return err
	}
	var resp map[string]any
	_ = json.Unmarshal(body, &resp)
	if outputJSON {
		return printJSON(resp)
	}
	if tok, ok := resp["token"].(string); ok {
		fmt.Println(tok)
		fmt.Fprintln(os.Stderr, "⚠ store this token now — it will not be shown again")
		return nil
	}
	return printJSON(resp)
}

func tokenRevoke(prefix string) error {
	if err := apiDELETE("/v1/me/tokens/" + url.PathEscape(prefix)); err != nil {
		return err
	}
	fmt.Printf("✓ revoked token %s\n", prefix)
	return nil
}

// ----------------------------------------------------------------------------
// Subcommand: function / schedule
// ----------------------------------------------------------------------------

type stringMapFlag map[string]string

func (m *stringMapFlag) String() string {
	if m == nil || len(*m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*m))
	for k, v := range *m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (m *stringMapFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return fmt.Errorf("invalid KEY=VAL: %q", value)
	}
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[strings.TrimSpace(parts[0])] = parts[1]
	return nil
}

type optionalBoolFlag struct {
	set   bool
	value bool
}

func (f *optionalBoolFlag) String() string {
	if !f.set {
		return ""
	}
	return strconv.FormatBool(f.value)
}

func (f *optionalBoolFlag) Set(value string) error {
	if value == "" {
		f.value = true
		f.set = true
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func (f *optionalBoolFlag) IsBoolFlag() bool { return true }

func functionCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("function requires a subcommand: list | deploy | get | update | delete | logs | run")
	}
	switch args[0] {
	case "list", "ls":
		return functionList()
	case "deploy":
		return functionDeploy(args[1:])
	case "get":
		if len(args) < 2 {
			return errors.New("function get ID")
		}
		return functionGet(args[1])
	case "update":
		return functionUpdate(args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			return errors.New("function delete ID")
		}
		return functionDelete(args[1])
	case "logs":
		if len(args) < 2 {
			return errors.New("function logs ID")
		}
		return functionLogs(args[1])
	case "run":
		if len(args) < 2 {
			return errors.New("function run ID")
		}
		return functionRun(args[1])
	default:
		return fmt.Errorf("unknown function subcommand: %s", args[0])
	}
}

func functionList() error {
	body, err := apiGET("/v1/functions")
	if err != nil {
		return err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("no functions")
		return nil
	}
	fmt.Printf("%-38s %-24s %-8s %-7s %-48s %s\n", "ID", "NAME", "RUNTIME", "PUBLIC", "ENDPOINT", "CREATED")
	for _, fn := range list {
		fmt.Printf("%-38v %-24v %-8v %-7v %-48v %v\n", fn["id"], fn["name"], fn["runtime"], fn["public"], functionEndpoint(fn), fn["created_at"])
	}
	return nil
}

func functionDeploy(args []string) error {
	fs := flag.NewFlagSet("function deploy", flag.ExitOnError)
	name := fs.String("name", "", "function name")
	runtime := fs.String("runtime", "", "runtime (python or nodejs)")
	template := fs.String("template", "code-interpreter", "template to run the function in")
	public := fs.Bool("public", false, "expose a public HTTP endpoint")
	var env stringMapFlag
	fs.Var(&env, "env", "env var (KEY=VAL, repeatable)")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 1 {
		return errors.New("function deploy FILE")
	}
	file := rest[0]
	code, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	resolvedRuntime := strings.TrimSpace(*runtime)
	if resolvedRuntime == "" {
		resolvedRuntime, err = inferFunctionRuntime(file)
		if err != nil {
			return err
		}
	}
	resolvedName := strings.TrimSpace(*name)
	if resolvedName == "" {
		resolvedName = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	}
	body, err := apiPOST("/v1/functions", map[string]any{
		"name":       resolvedName,
		"runtime":    resolvedRuntime,
		"entrypoint": filepath.Base(file),
		"code":       base64.StdEncoding.EncodeToString(code),
		"template":   *template,
		"env":        map[string]string(env),
		"public":     *public,
	})
	if err != nil {
		return err
	}
	var fn map[string]any
	if err := json.Unmarshal(body, &fn); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(fn)
	}
	fmt.Printf("✓ deployed function %v (%v)\n", fn["name"], fn["id"])
	if endpoint := functionEndpoint(fn); endpoint != "" && endpoint != "<nil>" {
		fmt.Printf("endpoint: %s\n", endpoint)
	}
	return nil
}

func functionGet(id string) error {
	body, err := apiGET("/v1/functions/" + url.PathEscape(id))
	if err != nil {
		return err
	}
	var fn any
	if err := json.Unmarshal(body, &fn); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return printJSON(fn)
}

func functionUpdate(args []string) error {
	if len(args) < 1 {
		return errors.New("function update ID")
	}
	id := args[0]
	fs := flag.NewFlagSet("function update", flag.ExitOnError)
	name := fs.String("name", "", "new function name")
	var public optionalBoolFlag
	var env stringMapFlag
	fs.Var(&env, "env", "env var (KEY=VAL, repeatable)")
	fs.Var(&public, "public", "set public endpoint state")
	_ = fs.Parse(args[1:])
	bodyReq := map[string]any{}
	if strings.TrimSpace(*name) != "" {
		bodyReq["name"] = *name
	}
	if len(env) > 0 {
		bodyReq["env"] = map[string]string(env)
	}
	if public.set {
		bodyReq["public"] = public.value
	}
	if len(bodyReq) == 0 {
		return errors.New("provide at least one of --name, --env, or --public")
	}
	body, err := apiDo("PATCH", "/v1/functions/"+url.PathEscape(id), bodyReq)
	if err != nil {
		return err
	}
	var fn any
	if err := json.Unmarshal(body, &fn); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(fn)
	}
	fmt.Printf("updated function %s\n", id)
	return printJSON(fn)
}

func functionDelete(id string) error {
	if err := apiDELETE("/v1/functions/" + url.PathEscape(id)); err != nil {
		return err
	}
	fmt.Printf("deleted function %s\n", id)
	return nil
}

func functionLogs(id string) error {
	body, err := apiGET("/v1/functions/" + url.PathEscape(id) + "/runs")
	if err != nil {
		return err
	}
	var runs []map[string]any
	if err := json.Unmarshal(body, &runs); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(runs)
	}
	if len(runs) == 0 {
		fmt.Println("no runs")
		return nil
	}
	fmt.Printf("%-38s %-10s %-9s %-11s %s\n", "RUN ID", "STATUS", "EXIT", "DURATION", "STARTED")
	for _, run := range runs {
		fmt.Printf("%-38v %-10v %-9v %-11v %v\n", run["id"], run["status"], run["exit_code"], run["duration_ms"], run["started_at"])
	}
	return nil
}

func functionRun(id string) error {
	body, err := apiPOST("/v1/functions/"+url.PathEscape(id)+"/runs", map[string]any{})
	if err != nil {
		return err
	}
	var run map[string]any
	if err := json.Unmarshal(body, &run); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(run)
	}
	if stdout, _ := run["stdout"].(string); stdout != "" {
		fmt.Fprint(os.Stdout, stdout)
	}
	if stderr, _ := run["stderr"].(string); stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
	fmt.Printf("run %v status=%v exit=%v duration_ms=%v\n", run["id"], run["status"], run["exit_code"], run["duration_ms"])
	return nil
}

func scheduleCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("schedule requires a subcommand: list | create | get | pause | resume | trigger | delete")
	}
	switch args[0] {
	case "list", "ls":
		return scheduleList()
	case "create":
		return scheduleCreate(args[1:])
	case "get":
		if len(args) < 2 {
			return errors.New("schedule get ID")
		}
		return scheduleGet(args[1])
	case "pause":
		if len(args) < 2 {
			return errors.New("schedule pause ID")
		}
		return scheduleSetPaused(args[1], true)
	case "resume":
		if len(args) < 2 {
			return errors.New("schedule resume ID")
		}
		return scheduleSetPaused(args[1], false)
	case "trigger":
		if len(args) < 2 {
			return errors.New("schedule trigger ID")
		}
		return scheduleTrigger(args[1])
	case "delete", "rm":
		if len(args) < 2 {
			return errors.New("schedule delete ID")
		}
		return scheduleDelete(args[1])
	default:
		return fmt.Errorf("unknown schedule subcommand: %s", args[0])
	}
}

func scheduleList() error {
	body, err := apiGET("/v1/schedules")
	if err != nil {
		return err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("no schedules")
		return nil
	}
	fmt.Printf("%-38s %-24s %-38s %-16s %-8s %-20s %s\n", "ID", "NAME", "FUNCTION", "CRON", "STATUS", "LAST RUN", "NEXT RUN")
	for _, sch := range list {
		status := "active"
		if paused, _ := sch["paused"].(bool); paused {
			status = "paused"
		}
		fmt.Printf("%-38v %-24v %-38v %-16v %-8s %-20v %v\n", sch["id"], sch["name"], sch["function_id"], sch["cron"], status, sch["last_run_at"], sch["next_run_at"])
	}
	return nil
}

func scheduleCreate(args []string) error {
	fs := flag.NewFlagSet("schedule create", flag.ExitOnError)
	name := fs.String("name", "", "schedule name (required)")
	functionID := fs.String("function-id", "", "function ID (required)")
	cron := fs.String("cron", "", "cron expression (required)")
	_ = fs.Parse(args)
	if *name == "" || *functionID == "" || *cron == "" {
		return errors.New("schedule create --name NAME --function-id ID --cron SPEC")
	}
	body, err := apiPOST("/v1/schedules", map[string]any{"name": *name, "function_id": *functionID, "cron": *cron, "paused": false})
	if err != nil {
		return err
	}
	var sch any
	if err := json.Unmarshal(body, &sch); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(sch)
	}
	fmt.Printf("created schedule %s for function %s\n", *name, *functionID)
	return printJSON(sch)
}

func scheduleGet(id string) error {
	body, err := apiGET("/v1/schedules/" + url.PathEscape(id))
	if err != nil {
		return err
	}
	var sch any
	if err := json.Unmarshal(body, &sch); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return printJSON(sch)
}

func scheduleSetPaused(id string, paused bool) error {
	body, err := apiDo("PATCH", "/v1/schedules/"+url.PathEscape(id), map[string]any{"paused": paused})
	if err != nil {
		return err
	}
	var sch any
	if err := json.Unmarshal(body, &sch); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(sch)
	}
	if paused {
		fmt.Printf("paused schedule %s\n", id)
	} else {
		fmt.Printf("resumed schedule %s\n", id)
	}
	return nil
}

func scheduleTrigger(id string) error {
	body, err := apiPOST("/v1/schedules/"+url.PathEscape(id)+"/trigger", map[string]any{})
	if err != nil {
		return err
	}
	var run any
	if err := json.Unmarshal(body, &run); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if outputJSON {
		return printJSON(run)
	}
	fmt.Printf("triggered schedule %s\n", id)
	return printJSON(run)
}

func scheduleDelete(id string) error {
	if err := apiDELETE("/v1/schedules/" + url.PathEscape(id)); err != nil {
		return err
	}
	fmt.Printf("deleted schedule %s\n", id)
	return nil
}

func inferFunctionRuntime(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python", nil
	case ".js", ".mjs", ".cjs", ".ts":
		return "nodejs", nil
	default:
		return "", fmt.Errorf("could not infer runtime from %q; pass --runtime python|nodejs", path)
	}
}

func functionEndpoint(fn map[string]any) any {
	if endpoint, ok := fn["endpoint"]; ok {
		return endpoint
	}
	if endpoint, ok := fn["url"]; ok {
		return endpoint
	}
	return ""
}
