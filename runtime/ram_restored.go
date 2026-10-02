package runtime

// RAMAlreadyRestored, when set, reports that the live RAM file already holds the guest image.
var RAMAlreadyRestored func(sandboxID string) bool
