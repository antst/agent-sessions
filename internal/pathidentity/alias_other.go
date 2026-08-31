//go:build !darwin

package pathidentity

func resolvePlatformPathAlias(string) (string, bool, error) {
	return "", false, nil
}
