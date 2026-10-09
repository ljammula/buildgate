package memory

import (
	"errors"
	"fmt"
)

// ErrMemoryOff wraps the refusal Gate returns; its message says which of the
// three stopped the action and how to change it.
var ErrMemoryOff = errors.New("memory is off")

// Gate is the one decision every memory action passes: the project's kill
// switch first, then the repository's switch in the session config, then the
// project's stop marker. It returns nil only when all three allow it.
func Gate(switchedOn, offMarker, killSwitchEngaged bool) error {
	switch {
	case killSwitchEngaged:
		return fmt.Errorf("%w: the project's kill switch is engaged; disengage it with `factoryd kill-switch -state disengaged -by <who> -reason <why>`", ErrMemoryOff)
	case !switchedOn:
		return fmt.Errorf("%w: this repository is not listed under memory.repositories in the session config", ErrMemoryOff)
	case offMarker:
		return fmt.Errorf("%w: the project's memory was stopped with `factoryd memory off`; resume it with `factoryd memory on`", ErrMemoryOff)
	}
	return nil
}
