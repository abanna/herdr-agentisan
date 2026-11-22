package notice

// AckSlots counts the callback positions that accompany a batcher's message.
func AckSlots(m Message) int { return len(m.acks) }
