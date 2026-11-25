package bossshift

// CheckModelWithReaders exposes the verifier with deterministic process and clock readers.
var CheckModelWithReaders = checkModel

// WaitBetweenReads runs a wait that calls between after each round's first read.
var WaitBetweenReads = waitBetween
