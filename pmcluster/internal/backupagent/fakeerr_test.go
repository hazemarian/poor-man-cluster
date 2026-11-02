package backupagent

import "errors"

// errFake stands in for a failing external command.
var errFake = errors.New("fake command failure")
