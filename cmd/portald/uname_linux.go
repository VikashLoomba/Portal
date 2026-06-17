//go:build linux

package main

import "syscall"

func init() {
	readKernel = func() string {
		var u syscall.Utsname
		if err := syscall.Uname(&u); err != nil {
			return ""
		}
		b := make([]byte, 0, 65)
		for _, c := range u.Release {
			if c == 0 {
				break
			}
			b = append(b, byte(c))
		}
		return string(b)
	}
}
