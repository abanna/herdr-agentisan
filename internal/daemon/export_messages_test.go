package daemon

// Refusal is the code of a prompt that certainly typed nothing, or "" for
// one that may have (A2).
var Refusal = refusal

// CleanText is text as the dispatcher types it (B6).
var CleanText = cleanText

// TextOf is what the dispatcher types a message as (Q9, B5).
var TextOf = textOf

// RecoveredReason is why a message a crash left dispatching failed.
const RecoveredReason = recoveredReason
