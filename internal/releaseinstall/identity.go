package releaseinstall

import (
	"errors"
)

// ContentIdentity hashes the exact no-follow bounded release tree except
// manifest.json. Types, paths, executable policy, sizes, and regular bytes are
// bound to the immutable projection used after staging.
func ContentIdentity(root string) (string, error) {
	source, err := openSecureReleaseSource(root)
	if err != nil {
		return "", errors.New("release identity root is not a real no-follow directory")
	}
	defer func() { _ = source.close() }()
	return secureReleaseContentIdentity(source)
}
