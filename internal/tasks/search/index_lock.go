package search

func notifyIndexLockWait(callbacks []func() error) error {
	for _, callback := range callbacks {
		if callback != nil {
			return callback()
		}
	}
	return nil
}
