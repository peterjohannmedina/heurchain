package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
)

const brokerInstanceIDPath = "/etc/heurchain/instance-id"

// brokerCommit is replaced at link time for release builds. Local builds keep
// the explicit development value so health responses never imply provenance
// they do not have.
var brokerCommit = "development"

var (
	brokerSHAOnce sync.Once
	brokerSHA     = "unknown"
)

func brokerArtifactSHA256() string {
	brokerSHAOnce.Do(func() {
		executable, err := os.Executable()
		if err != nil {
			return
		}
		data, err := os.ReadFile(executable)
		if err != nil {
			return
		}
		digest := sha256.Sum256(data)
		brokerSHA = hex.EncodeToString(digest[:])
	})
	return brokerSHA
}

func brokerInstanceID() string {
	if value := strings.TrimSpace(os.Getenv("HEURCHAIN_INSTANCE_ID")); value != "" {
		return value
	}
	data, err := os.ReadFile(brokerInstanceIDPath)
	if err != nil {
		return "unconfigured"
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "unconfigured"
	}
	return value
}
