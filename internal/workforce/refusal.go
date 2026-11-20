package workforce

// The states of a retry key the daemon answers with (A73): free when
// nothing was spawned under it, open while workers may exist and its
// request is not settled, spent once its request is finished.
const (
	KeyFree  = "free"
	KeyOpen  = "open"
	KeySpent = "spent"
)

// RefusalError is a keyed spawn's refusal as the daemon answers it (A73): the
// code and message its rule computed once (Service.Refusal), and the retry
// key with its state, which only it carries. Err is its cause, or
// ErrReplayed for a stored answer replayed, whose cause is gone.
type RefusalError struct {
	Code, Message, RetryKey, KeyState string
	Err                               error
}

// Error is the refusal's message.
func (r *RefusalError) Error() string { return r.Message }

// Unwrap is the refusal's cause, so its sentinel still matches.
func (r *RefusalError) Unwrap() error { return r.Err }
