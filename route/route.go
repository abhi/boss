package route

import (
	"net"
	"os/exec"

	"github.com/pkg/errors"
)

const Interface = "mvlan0"

func Create(iface, address string) (err error) {
	// don't create if it already exists
	if _, err := net.InterfaceByName(Interface); err == nil {
		return nil
	}
	defer func() {
		if err != nil {
			ip("link", "del", Interface)
		}
	}()
	if err := ip("link", "add", "link", iface, Interface, "type", "macvlan", "mode", "bridge"); err != nil {
		return err
	}
	if err := ip("address", "add", address, "dev", Interface); err != nil {
		return err
	}
	if err := ip("link", "set", "dev", Interface, "up"); err != nil {
		return err
	}
	return ip("route", "flush", "dev", Interface)
}

func Add(address string) error {
	return ip("route", "add", address, "dev", Interface, "metric", "0")
}

func Remove(address string) error {
	return ip("route", "del", address, "dev", Interface)
}

func ip(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return errors.Wrap(err, string(out))
	}
	return nil
}
