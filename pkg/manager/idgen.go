package manager

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

const idEntropyBytes = 16

type randomIDGenerator struct{}

func (randomIDGenerator) PluginID() PluginID {
	return PluginID("plg_" + randomToken())
}

func (randomIDGenerator) WorkspaceID() WorkspaceID {
	return WorkspaceID("wsp_" + randomToken())
}

func randomToken() string {
	buf := make([]byte, idEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("manager id generator: %v", err))
	}

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return strings.ToLower(encoded)
}
