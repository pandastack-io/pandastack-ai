# pandastack CLI

Stdlib-only Go CLI for PandaStack microVM sandboxes.

## Configuration

```sh
export PANDASTACK_API=https://api.pandastack.ai
export PANDASTACK_TOKEN=pds_...
```

Or log in once and let the CLI save `~/.config/pandastack/config.json`:

```sh
pandastack auth login
pandastack auth whoami
pandastack auth logout
```

Use `-o json` with list/get-style commands:

```sh
pandastack -o json sandbox list
pandastack -o json sandbox get sb_123
pandastack -o json template list
pandastack -o json version
```

## Auth

```sh
pandastack auth login
pandastack auth whoami
pandastack auth logout
```

`auth login` prompts for email/password, signs in with Supabase, exchanges the session JWT for a long-lived PandaStack account token, and saves it locally. Supabase can be overridden with `PANDASTACK_SUPABASE_URL` and `PANDASTACK_SUPABASE_ANON_KEY`.

## Templates

```sh
pandastack template list
pandastack template build -f Dockerfile -n python --size-mb 2048 --context .
pandastack template delete python
```

`template build` uses Docker to build an image, export a rootfs tar, upload it to `/v1/templates/build`, and poll until the build finishes.

## Sandboxes

```sh
pandastack sandbox create --template python --ttl 1h
pandastack sandbox list
pandastack sandbox get sb_123
pandastack sandbox delete sb_123
```

Lifecycle helpers:

```sh
pandastack sandbox pause sb_123
pandastack sandbox resume sb_123
pandastack sandbox snapshot sb_123
```

Run commands:

```sh
pandastack sandbox exec sb_123 -- uname -a
pandastack sandbox exec sb_123 --timeout 30 -- python -c 'print("hello")'
```

Stream logs:

```sh
pandastack sandbox logs sb_123
pandastack sandbox logs sb_123 --no-follow
pandastack sandbox logs sb_123 --stream stdout
pandastack sandbox logs sb_123 --stream stderr
```

Copy files:

```sh
pandastack sandbox cp ./app.py sb_123:/root/app.py
pandastack sandbox cp sb_123:/root/output.txt ./output.txt
pandastack sandbox cp sb_123:/root/output.txt .
```

SSH from a network that can route to the guest IP:

```sh
pandastack sandbox ssh sb_123
```

If the guest IP is private to the PandaStack cluster, use `pandastack sandbox exec` instead or run SSH on-cluster.

## Version and help

```sh
pandastack version
pandastack help
```

`version` prints both client and server versions and warns when their major.minor versions differ.
