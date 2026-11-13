package bluge

import "encoding/hex"

const (
	contentField   = "c"
	kindField      = "k"
	createdAtField = "a"
	pubkeyField    = "p"
)

type eventIdentifier string

const idField = "i"

func (id eventIdentifier) Field() string {
	return idField
}

func (id eventIdentifier) Term() []byte {
	v, _ := hex.DecodeString(string(id))
	return v
}
