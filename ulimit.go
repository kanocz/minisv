package main

import (
	"errors"
	"fmt"
	"log"
	"syscall"
)

type configRLimit struct {
	Type string `json:"type"`
	Cur  uint64 `json:"cur"`
	Max  uint64 `json:"max"`
}

var (
	errInvalidRLimit = errors.New("type not defined for current platform")
)

func setLimit(limit configRLimit) error {
	id, ok := rlimitTypes[limit.Type]
	if !ok {
		return errInvalidRLimit
	}

	rLimit := syscall.Rlimit{
		Cur: limit.Cur,
		Max: limit.Max,
	}

	err := syscall.Setrlimit(id, &rLimit)
	if err != nil {
		return err
	}

	err = syscall.Getrlimit(id, &rLimit)
	if err != nil {
		return err
	}

	if rLimit.Cur != limit.Cur || rLimit.Max != limit.Max {
		return fmt.Errorf("try to set %d/%d, but got %d/%d",
			limit.Cur, limit.Max, rLimit.Cur, rLimit.Max)
	}

	return nil
}

func processRLimits(limits []configRLimit) {
	for _, limit := range limits {
		err := setLimit(limit)
		if nil != err {
			log.Println("Error setting limit for \""+limit.Type+"\":", err)
		}
	}
}
