package proto

import "crypto/sha256"

const infoPrefix = "airgapp-gw/1\x00"

// Info is the HPKE info: prefix || SHA-256(hello || challenge), 45 bytes.
func Info(hello, challenge []byte) []byte {
	h := sha256.New()
	h.Write(hello)
	h.Write(challenge)
	return h.Sum([]byte(infoPrefix))
}
