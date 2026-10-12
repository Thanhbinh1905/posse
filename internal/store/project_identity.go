package store

import (
	"crypto/rand"
	"encoding/hex"
)

func newProjectUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
