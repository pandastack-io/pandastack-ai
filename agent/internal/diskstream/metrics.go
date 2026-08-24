// SPDX-License-Identifier: Apache-2.0
package diskstream

// Metrics hooks. diskstream is deliberately dependency-free (no import of the
// agent's obs/prometheus package), so it exposes package-level hook variables
// that the agent wires to obs counters at startup (see sandbox wiring). Until
// wired they are no-ops, so the package stays usable standalone (and in tests).
//
// Increments happen at the MOMENT each event occurs — not sampled from Stats()
// at teardown — so a closed/recovered device's counts are never lost. This is
// the lost-counts fix the Phase-3 critique called for, achieved without a pull
// collector: fetch_bytes (billing-critical) is recorded as bytes actually leave
// for GCS, regardless of device lifecycle.
var (
	// OnFetch is called once per chunk successfully fetched from the upstream
	// into a cache (resolver or shared cache). For the chunk-count metric.
	OnFetch func()
	// OnZeroFill is called once per device read served from an absent/zero chunk.
	OnZeroFill func()
	// OnFetchBytes is called with the byte count + (template, generation) each
	// time bytes are pulled from GCS through the egress governor — the
	// billing/abuse signal. template/generation may be "" when unlabeled.
	OnFetchBytes func(template, generation string, n int64)
	// OnVerifyError is called when a fetched chunk fails its SHA-256 check.
	OnVerifyError func()
	// OnGCSRetry is called on each retried transient GCS fetch error.
	OnGCSRetry func()
	// OnCapHit is called when the egress lifetime hard cap rejects a read.
	OnCapHit func()
)

// label carries the (template, generation) a govSource charges fetch bytes
// under. Set on construction; empty when the caller did not supply it.
type fetchLabel struct {
	template   string
	generation string
}

func fireFetch() {
	if OnFetch != nil {
		OnFetch()
	}
}
func fireZeroFill() {
	if OnZeroFill != nil {
		OnZeroFill()
	}
}
func fireFetchBytes(l fetchLabel, n int64) {
	if OnFetchBytes != nil && n > 0 {
		OnFetchBytes(l.template, l.generation, n)
	}
}
func fireVerifyError() {
	if OnVerifyError != nil {
		OnVerifyError()
	}
}
func fireCapHit() {
	if OnCapHit != nil {
		OnCapHit()
	}
}
func fireGCSRetry() {
	if OnGCSRetry != nil {
		OnGCSRetry()
	}
}
