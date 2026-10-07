//go:build !unix

package cfg

import "errors"

func lockFile(string) (func(), error) {
	return nil, errors.New("agentcfg needs a Unix system (file locking)")
}
