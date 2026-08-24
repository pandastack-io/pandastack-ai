// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Managed-database credential rotation (roadmap P2: password reset).
//
// Rotates BOTH secrets of a RUNNING database — the postgres password and the
// query-broker bearer token — by replaying exactly what autostart.sh phase 2
// does at boot: ALTER USER, persist the new values under /etc/pandastack,
// refresh the pgbouncer userlist, restart the broker (it reads its token file
// and PG password env only at startup), and rewrite /run/pandastack/ready.json
// so the control plane's next pg-info fetch serves the new credentials.
//
// Idempotent by construction: every step overwrites state derived from the
// same new secret pair, so a mid-sequence failure is fixed by calling the
// rotation again. Until ready.json is rewritten the API may briefly serve the
// OLD password — which stops working at the ALTER — so the caller (control
// plane) treats any error as "rotation incomplete, retry", never success.

// RotateDBCredentials generates a fresh postgres password + broker token and
// installs them in the running guest. The new values are NOT returned; the
// control plane re-reads ready.json (the single source of truth for DB
// credentials) after this returns.
func (m *Manager) RotateDBCredentials(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("db rotate: invalid database id %q", id)
	}
	if st, err := m.store.GetStatus(ctx, id); err != nil || st != string(StatusRunning) {
		return fmt.Errorf("db rotate: database is not running (status %q)", st)
	}
	drv := m.driver(id)
	if drv == nil || !pidAlive(drv.PID()) {
		return fmt.Errorf("db rotate: database VM is not alive on this host")
	}

	password, err := generateToken(24)
	if err != nil {
		return fmt.Errorf("db rotate: generate password: %w", err)
	}
	rawTok, err := generateToken(24)
	if err != nil {
		return fmt.Errorf("db rotate: generate broker token: %w", err)
	}
	brokerToken := "pds_pg_" + rawTok

	gc, err := m.Guest(id)
	if err != nil {
		return fmt.Errorf("db rotate: guest unreachable: %w", err)
	}

	// Tokens are base64url (A-Za-z0-9-_): safe inside single- AND
	// double-quoted shell strings, and inside the SQL string literal.
	// Steps are (label, cmd) pairs — failures log the LABEL only; the
	// commands embed plaintext secrets and must never reach journald.
	type rotateStep struct{ label, cmd string }
	steps := []rotateStep{
		{"alter-user",
			fmt.Sprintf(`sudo -u postgres psql -v ON_ERROR_STOP=1 -c "ALTER USER pandastack WITH PASSWORD '%s';" >/dev/null 2>&1`, password)},
		{"persist-password",
			fmt.Sprintf(`printf '%%s' '%s' > /etc/pandastack/pg.password && chmod 600 /etc/pandastack/pg.password`, password)},
		// pgbouncer: scram verifier from pg_shadow (plaintext fallback),
		// then SIGHUP to reload the auth file. Written via > so the file
		// keeps its template-time ownership (pgbouncer must stay able to
		// read it). Reload is best-effort — matches autostart's "non-fatal"
		// treatment of pgbouncer.
		{"pgbouncer-userlist",
			fmt.Sprintf(`PG_HASH=$(sudo -u postgres psql -tA -c "SELECT passwd FROM pg_shadow WHERE usename='pandastack'" 2>/dev/null | head -1); `+
				`if [ -n "$PG_HASH" ]; then printf '"pandastack" "%%s"\n' "$PG_HASH" > /etc/pgbouncer/userlist.txt; `+
				`else printf '"pandastack" "%%s"\n' '%s' > /etc/pgbouncer/userlist.txt; fi; `+
				`chmod 640 /etc/pgbouncer/userlist.txt; `+
				`kill -HUP $(cat /var/run/pgbouncer/pgbouncer.pid 2>/dev/null) 2>/dev/null || true`, password)},
		{"persist-broker-token",
			fmt.Sprintf(`printf '%%s' '%s' > /etc/pandastack/broker.token && chmod 600 /etc/pandastack/broker.token`, brokerToken)},
		// broker.env carries PG_PASSWORD — the broker authenticates to
		// postgres with it and reads it only at startup.
		{"broker-env",
			fmt.Sprintf(`printf 'PG_HOST=127.0.0.1\nPG_PORT=5432\nPG_USER=pandastack\nPG_PASSWORD=%s\nPG_SSLMODE=disable\nBROKER_TOKEN_FILE=/etc/pandastack/broker.token\nBROKER_ADDR=0.0.0.0:5544\nBROKER_METRICS_ADDR=127.0.0.1:5545\n' > /etc/pandastack/broker.env && chmod 600 /etc/pandastack/broker.env`, password)},
		// setsid + </dev/null so the new broker survives this SSH session.
		// Wait for the OLD broker to actually exit before starting the new
		// one: both bind 0.0.0.0:5544, and a start that loses the race dies
		// on the bind, leaving the tenant's REST API down.
		{"broker-restart",
			`OLDPID=$(cat /run/pandastack/broker.pid 2>/dev/null); ` +
				`if [ -n "$OLDPID" ]; then kill "$OLDPID" 2>/dev/null || true; ` +
				`for i in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$OLDPID" 2>/dev/null || break; sleep 0.3; done; fi; ` +
				`rm -f /run/pandastack/broker.pid; ` +
				`set -a; . /etc/pandastack/broker.env; set +a; ` +
				`setsid /usr/local/bin/pds-query-broker >>/var/log/pds-query-broker.log 2>&1 </dev/null & echo $! > /run/pandastack/broker.pid`},
		// ready.json is what fetchPGInfo serves to the control plane. Same
		// field set autostart writes; started_at reflects the rotation. The
		// JSON body lives in the single-quoted printf FORMAT string (secrets
		// are base64url — no quotes to collide); only the date is an arg.
		{"ready-json",
			fmt.Sprintf(`printf '{\n  "status": "ready",\n  "broker_url": "http://127.0.0.1:5544",\n  "broker_token": "%s",\n  "pg_host": "127.0.0.1",\n  "pg_port": 5432,\n  "pg_user": "pandastack",\n  "pg_password": "%s",\n  "pgbouncer_port": 6432,\n  "default_database": "pandastack",\n  "started_at": "%%s"\n}\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" > /run/pandastack/ready.json && chmod 644 /run/pandastack/ready.json`, brokerToken, password)},
		// Verify both paths actually work with the NEW secrets before
		// reporting success: a direct SQL auth and a tokened broker query
		// (bounded wait — the broker just restarted).
		{"verify-postgres",
			fmt.Sprintf(`PGPASSWORD='%s' psql -h 127.0.0.1 -U pandastack -d pandastack -tA -c 'SELECT 1' >/dev/null`, password)},
		{"verify-broker",
			fmt.Sprintf(`for i in $(seq 1 15); do curl -sf -X POST -H 'Authorization: Bearer %s' -H 'Content-Type: application/json' -d '{"database":"pandastack","sql":"SELECT 1"}' http://127.0.0.1:5544/v1/query >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1`, brokerToken)},
	}

	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for _, st := range steps {
		var lastErr error
		done := false
		for attempt := 0; attempt < 3; attempt++ {
			res, execErr := gc.Exec(rctx, st.cmd)
			if execErr == nil && res.ExitCode == 0 {
				done = true
				break
			}
			if execErr != nil {
				lastErr = execErr
			} else {
				lastErr = fmt.Errorf("exit %d", res.ExitCode)
			}
			time.Sleep(300 * time.Millisecond)
		}
		if !done {
			m.log.Error("db rotate: step failed", "id", id, "step", st.label, "err", lastErr)
			return fmt.Errorf("db rotate: step %s failed: %w (rotation incomplete — retry)", st.label, lastErr)
		}
	}

	if m.bus != nil {
		m.bus.Emit(id, "database.credentials_rotated", map[string]any{})
	}
	m.log.Info("db rotate: credentials rotated and verified", "id", id)
	return nil
}
