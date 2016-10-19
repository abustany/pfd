// +build !linux

package main

func GetTerminalWidth() (uint, error) {
	return 80, nil
}
