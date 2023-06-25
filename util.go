package gowest

import (
	"crypto/sha1"
	"encoding/base64"
	"strings"
)

func tokenPresentInString(s string, t string) bool {
	tokens := strings.Fields(s)
	for _, token := range tokens {
		if t == token {
			return true
		}
	}
	return false
}

func wsSecKey(key []byte) string {
	sha := sha1.New()
	sha.Write(key)
	sha.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(sha.Sum(nil))
}
