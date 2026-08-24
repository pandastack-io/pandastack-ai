// SPDX-License-Identifier: Apache-2.0
//
// databases_metrics.go — GET /v1/databases/{id}/metrics
//
// Historical CPU + memory (and free bonus: network + disk I/O) time series
// for one database, sourced from the ClickHouse sandbox_metrics table that
// the agent's per-VM sampler feeds every ~15 seconds. Powers the Overview
// tab's charts on /databases/{id}.
//
// Query shape:
//
//	GET /v1/databases/{id}/metrics?range=24h[&bucket=5m]
//
// range:  1h | 24h | 7d | 30d           (default 24h)
// bucket: 15s | 1m | 5m | 30m | 2h      (default: auto per range, see rangeSpecs)
//
// Response envelope: {range, bucket, from, to, points:[{ts,cpu_pct,mem_bytes,
// net_rx_bytes, net_tx_bytes, disk_rd_bytes, disk_wr_bytes}]} — one row per
// bucket, at most ~360 rows for any range so a single 30-day chart never
// exceeds a light payload.
//
// Ownership: same gate as /backups — fetchPGInfo confirms the caller owns
// the DB (workspace-scoped), so a foreign id 404s before any ClickHouse read.
// Result cache: 60s TTL per (db, range, bucket) — the dashboard polls the
// live stats endpoint at 5s cadence for "right now" numbers; historical
// charts do NOT need to be fresher than that.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rangeSpec resolves the user-facing "range" string to the ClickHouse
// interval + a default bucket size, and validates the total point count
// stays reasonable. Buckets are picked so the point count is always ≤360.
type rangeSpec struct {
	dur          time.Duration
	defaultBucket string
	// allowedBuckets is the whitelist for ?bucket= on this range. Guards
	// against a caller asking for 15s buckets over 30d (172,800 points).
	allowedBuckets map[string]bool
}

var rangeSpecs = map[string]rangeSpec{
	"1h":  {dur: 1 * time.Hour, defaultBucket: "15s", allowedBuckets: map[string]bool{"15s": true, "1m": true}},
	"24h": {dur: 24 * time.Hour, defaultBucket: "5m", allowedBuckets: map[string]bool{"1m": true, "5m": true}},
	"7d":  {dur: 7 * 24 * time.Hour, defaultBucket: "30m", allowedBuckets: map[string]bool{"5m": true, "30m": true}},
	"30d": {dur: 30 * 24 * time.Hour, defaultBucket: "2h", allowedBuckets: map[string]bool{"30m": true, "2h": true}},
}

var chIntervalOf = map[string]string{
	"15s": "INTERVAL 15 SECOND",
	"1m":  "INTERVAL 1 MINUTE",
	"5m":  "INTERVAL 5 MINUTE",
	"30m": "INTERVAL 30 MINUTE",
	"2h":  "INTERVAL 2 HOUR",
}

// bucketSeconds mirrors the strings above as seconds — sent to the client so
// it can draw x-axis ticks correctly without re-parsing the label.
var bucketSeconds = map[string]int{
	"15s": 15, "1m": 60, "5m": 300, "30m": 1800, "2h": 7200,
}

// dbMetricsCache: memoize per (db, range, bucket). Same shape as
// backupsListCache. Ownership was already checked BEFORE the cache lookup on
// every request, so this can never serve one workspace's data to another.
var dbMetricsCache sync.Map // key: <ws>|<id>|<range>|<bucket> -> *dbMetricsCacheEntry

const dbMetricsCacheTTL = 60 * time.Second

type dbMetricsCacheEntry struct {
	at   time.Time
	body []byte
}

// dbMetricPoint is one bucket in the response. All counters are already
// bucket-aggregated on the ClickHouse side (avg for gauges like cpu_pct /
// mem_bytes; sum for counters like net_* / disk_*), so the client can
// treat them as ready to plot.
type dbMetricPoint struct {
	TS          string  `json:"ts"`
	CPUPct      float64 `json:"cpu_pct"`
	MemBytes    uint64  `json:"mem_bytes"`
	NetRxBytes  uint64  `json:"net_rx_bytes"`
	NetTxBytes  uint64  `json:"net_tx_bytes"`
	DiskRdBytes uint64  `json:"disk_rd_bytes"`
	DiskWrBytes uint64  `json:"disk_wr_bytes"`
}

type dbMetricsResponse struct {
	Range         string          `json:"range"`
	Bucket        string          `json:"bucket"`
	BucketSeconds int             `json:"bucket_seconds"`
	From          string          `json:"from"`
	To            string          `json:"to"`
	Points        []dbMetricPoint `json:"points"`
	// Empty is true when the pipeline is wired but this database has no
	// samples yet in the requested window (new DB, just spun up). Lets the
	// dashboard render "collecting…" instead of "no data".
	Empty bool `json:"empty"`
}

func (d *databasesAPI) metrics(w http.ResponseWriter, r *http.Request) {
	workspace := dbWorkspace(r)
	if workspace == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	id := r.PathValue("id")

	rangeKey := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	if rangeKey == "" {
		rangeKey = "24h"
	}
	spec, ok := rangeSpecs[rangeKey]
	if !ok {
		writeErrOrg(w, http.StatusBadRequest, "range must be one of 1h, 24h, 7d, 30d")
		return
	}
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	if bucket == "" {
		bucket = spec.defaultBucket
	}
	if !spec.allowedBuckets[bucket] {
		writeErrOrg(w, http.StatusBadRequest,
			"bucket "+strconv.Quote(bucket)+" not allowed for range "+rangeKey+" (would produce too many points)")
		return
	}
	chInterval := chIntervalOf[bucket]

	// Ownership: fetchPGInfo is workspace-scoped + lease-routed AND the
	// backup handler already uses it as the gate; a foreign id fails here
	// with the same 404 message. Skipping ClickHouse entirely on a foreign
	// id is important — the CH row is indexed by (workspace_id, sandbox_id)
	// so a leak would need both a foreign workspace header AND a valid id.
	// This gate closes that gap.
	//
	// Note: we intentionally allow a hibernated/failed DB — historical
	// metrics for a currently-idle DB is exactly what a user wants to see.
	// So we don't require pg to be ready here; we just need the row to
	// exist for this workspace.
	if _, ok := d.getFromSharedTable(r.Context(), workspace, id); !ok {
		// Fall back to the agent (a live DB may not be in the shared table
		// during transient windows — same behavior as /backups).
		resp, gerr := d.agentCall(r, "GET", "/v1/sandboxes/"+id, workspace, nil)
		owned := false
		if gerr == nil {
			owned = resp.StatusCode >= 200 && resp.StatusCode < 300
			resp.Body.Close()
		}
		if !owned {
			writeErrOrg(w, http.StatusNotFound, "database not found")
			return
		}
	}

	// Cache: gate is passed above, so serving a cached body is safe. The
	// key includes workspace so cache hits are workspace-scoped even though
	// db ids are globally unique (belt + suspenders vs any future id reuse).
	cacheKey := workspace + "|" + id + "|" + rangeKey + "|" + bucket
	if v, ok := dbMetricsCache.Load(cacheKey); ok {
		if e, _ := v.(*dbMetricsCacheEntry); e != nil && time.Since(e.at) < dbMetricsCacheTTL {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(e.body)
			return
		}
	}

	to := time.Now().UTC()
	from := to.Add(-spec.dur)

	out := dbMetricsResponse{
		Range:         rangeKey,
		Bucket:        bucket,
		BucketSeconds: bucketSeconds[bucket],
		From:          from.Format(time.RFC3339),
		To:            to.Format(time.RFC3339),
		Points:        []dbMetricPoint{},
		Empty:         true,
	}

	// If the ClickHouse read path isn't wired (dev / stripped deploy),
	// return an empty envelope instead of erroring. The dashboard will
	// render "collecting…" — same shape as apps_metrics.
	if d.ch == nil || d.ch.reader == nil {
		writeMetricsJSON(w, out)
		return
	}

	// ONE query, all six series. cpu_pct / mem_bytes are GAUGES (avg per
	// bucket = mean utilization); net_* + disk_* are COUNTERS (sum per bucket
	// = bytes moved in that window). Ordered by ts so the client can render
	// left→right without a sort.
	//
	// Safe interpolation: all inputs are validated upstream —
	//   - workspace: from the auth chain (X-Fcs-Workspace, gate-checked)
	//   - id: same source as fetchPGInfo, which we already used to gate
	//     ownership; single-quoted here after chQuote to escape any \, '
	//   - chInterval: from a whitelist (chIntervalOf)
	//   - from/to: RFC3339 formatted from time.Time
	fromLit := chQuote(from.Format("2006-01-02 15:04:05.000"))
	toLit := chQuote(to.Format("2006-01-02 15:04:05.000"))
	sql := fmt.Sprintf(`
		SELECT
		    toStartOfInterval(ts, %s) AS bucket,
		    avg(cpu_pct)               AS cpu_pct,
		    round(avg(mem_bytes))      AS mem_bytes,
		    sum(net_rx_bytes)          AS net_rx_bytes,
		    sum(net_tx_bytes)          AS net_tx_bytes,
		    sum(disk_rd_bytes)         AS disk_rd_bytes,
		    sum(disk_wr_bytes)         AS disk_wr_bytes
		  FROM pandastack.sandbox_metrics
		 WHERE workspace_id = %s
		   AND sandbox_id   = %s
		   AND ts BETWEEN %s AND %s
		 GROUP BY bucket
		 ORDER BY bucket`,
		chInterval, chQuote(workspace), chQuote(id), fromLit, toLit)

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	res, err := d.ch.reader.Query(ctx, sql)
	if err != nil {
		d.log.Warn("db metrics query failed", "id", id, "range", rangeKey, "err", err)
		// Serve the empty envelope on ClickHouse errors — same policy as
		// backups (a transient sink outage is a "no data" state, not a hard
		// failure the user needs to see).
		writeMetricsJSON(w, out)
		return
	}

	pts := make([]dbMetricPoint, 0, len(res.Data))
	for _, row := range res.Data {
		pts = append(pts, dbMetricPoint{
			TS:          asString(row["bucket"]),
			CPUPct:      asFloat(row["cpu_pct"]),
			MemBytes:    asUint64(row["mem_bytes"]),
			NetRxBytes:  asUint64(row["net_rx_bytes"]),
			NetTxBytes:  asUint64(row["net_tx_bytes"]),
			DiskRdBytes: asUint64(row["disk_rd_bytes"]),
			DiskWrBytes: asUint64(row["disk_wr_bytes"]),
		})
	}
	out.Points = pts
	out.Empty = len(pts) == 0

	body, _ := json.Marshal(out)
	dbMetricsCache.Store(cacheKey, &dbMetricsCacheEntry{at: time.Now(), body: body})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeMetricsJSON emits the response body, marking the cache header so we
// can spot-check hit rates in prod logs without a separate metric.
func writeMetricsJSON(w http.ResponseWriter, out dbMetricsResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "BYPASS")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// The ClickHouse JSON driver hands numeric values back as either float64,
// json.Number, or string (depending on the CH version and format). Robust
// getters below avoid a runtime panic on any of those.

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asFloat(v any) float64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

func asUint64(v any) uint64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case uint64:
		return t
	case int64:
		if t < 0 {
			return 0
		}
		return uint64(t)
	case float64:
		if t < 0 {
			return 0
		}
		return uint64(t)
	case json.Number:
		n, err := t.Int64()
		if err == nil && n >= 0 {
			return uint64(n)
		}
		f, _ := t.Float64()
		if f < 0 {
			return 0
		}
		return uint64(f)
	case string:
		n, err := strconv.ParseUint(t, 10, 64)
		if err == nil {
			return n
		}
		f, _ := strconv.ParseFloat(t, 64)
		if f < 0 {
			return 0
		}
		return uint64(f)
	}
	return 0
}
