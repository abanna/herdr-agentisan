//go:build !linux && !darwin

package report

// systemReaders has no process table to read here, so SelfLineage returns
// nil and a report trusts HERDR_PANE_ID unchecked.
func systemReaders() (StatFunc, CmdlineFunc) {
	return nil, nil
}
