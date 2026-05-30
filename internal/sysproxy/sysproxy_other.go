//go:build !darwin && !windows && !linux

package sysproxy

func services() ([]string, error)                 { return nil, ErrUnsupported }
func enable(service, host string, port int) error { return ErrUnsupported }
func disable(service string) error                { return ErrUnsupported }
func cleanup() error                              { return nil }
