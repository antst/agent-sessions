package pifamily

// PermissionPolicy is the immutable native policy selected at process launch.
// Args is always a fresh slice so callers cannot mutate a shared policy row.
type PermissionPolicy struct {
	Name string
	Args []string
}

func NewPermissionPolicy(name string, arguments ...string) PermissionPolicy {
	return PermissionPolicy{Name: name, Args: append([]string(nil), arguments...)}
}
