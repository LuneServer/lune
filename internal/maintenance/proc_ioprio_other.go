//go:build !linux

package maintenance

func lowerIOPriority(int) {}
