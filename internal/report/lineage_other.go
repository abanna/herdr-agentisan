//go:build !linux

package report

// procfs is false off Linux: there is no /proc to read a lineage from, so
// SelfLineage returns nil and a report trusts HERDR_PANE_ID unchecked.
const procfs = false
