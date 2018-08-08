package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/contrib/apparmor"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/typeurl"
	"github.com/crosbymichael/boss/config"
	"github.com/crosbymichael/boss/system"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

var (
	errIDRequired     = errors.New("container id is required")
	errUnableToSignal = errors.New("unable to signal task")
)

var systemdCommand = cli.Command{
	Name:   "systemd",
	Usage:  "systemd proxy for containers",
	Hidden: true,
	Subcommands: []cli.Command{
		systemdExecStartPreCommand,
		systemdExecStartCommand,
		systemdExecStartPostCommand,
	},
}

var systemdExecStartPreCommand = cli.Command{
	Name:   "exec-start-pre",
	Usage:  "exec-start-pre proxy for containers",
	Before: systemdPreSetup,
	Action: func(clix *cli.Context) error {
		id := clix.Args().First()
		if err := setupResolvConf(); err != nil {
			return err
		}
		if err := setupApparmor(); err != nil {
			return err
		}
		return cleanupPreviousTask(id)
	},
}

var systemdExecStartPostCommand = cli.Command{
	Name:   "exec-start-post",
	Usage:  "exec-start-post proxy for containers",
	Before: systemdPreSetup,
	Action: func(clix *cli.Context) error {
		id := clix.Args().First()
		err := cleanupPreviousTask(id)
		if merr := cfg.GetRegister().EnableMaintainance(id, "task exited"); err == nil {
			err = merr
		}
		return err
	},
}

var systemdExecStartCommand = cli.Command{
	Name:   "exec-start",
	Usage:  "exec-start proxy for containers",
	Before: systemdPreSetup,
	Action: func(clix *cli.Context) error {
		id := clix.Args().First()
		var (
			signals = make(chan os.Signal, 64)
			ctx     = cfg.Context()
			client  = cfg.Client()
		)
		signal.Notify(signals)
		container, err := client.LoadContainer(ctx, id)
		if err != nil {
			return err
		}
		config, err := getConfig(ctx, container)
		if err != nil {
			return err
		}
		task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
		if err != nil {
			return err
		}
		if err := setupNetworking(ctx, task, config); err != nil {
			task.Delete(ctx, containerd.WithProcessKill)
			return err
		}
		status, err := monitorTask(ctx, client, task, signals)
		if err != nil {
			return err
		}
		os.Exit(status)
		return nil
	},
}

func monitorTask(ctx context.Context, client *containerd.Client, task containerd.Task, signals chan os.Signal) (int, error) {
	defer task.Delete(ctx, containerd.WithProcessKill)
	started := make(chan error, 1)
	wait, err := task.Wait(ctx)
	if err != nil {
		return -1, err
	}
	go func() {
		started <- task.Start(ctx)
	}()
	for {
		select {
		case err := <-started:
			if err != nil {
				return -1, err
			}
			if err := cfg.GetRegister().DisableMaintainance(task.ID()); err != nil {
				logrus.WithError(err).Error("disable service maintenance")
			}
		case s := <-signals:
			if err := trySendSignal(ctx, client, task, s); err != nil {
				logrus.WithError(err).Error("signal task")
			}
		case exit := <-wait:
			if exit.Error() != nil {
				if !isUnavailable(err) {
					return -1, err
				}
				if err := reconnect(client); err != nil {
					return -1, err
				}
				if wait, err = task.Wait(ctx); err != nil {
					return -1, err
				}
				continue
			}
			return int(exit.ExitCode()), nil
		}
	}
}

func trySendSignal(ctx context.Context, client *containerd.Client, task containerd.Task, s os.Signal) error {
	for i := 0; i < 5; i++ {
		err := task.Kill(ctx, s.(syscall.Signal))
		if err == nil {
			return nil
		}
		if !isUnavailable(err) {
			return err
		}
		if err := reconnect(client); err != nil {
			return err
		}
	}
	return errUnableToSignal
}

func reconnect(client *containerd.Client) (err error) {
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if err = client.Reconnect(); err == nil {
			return nil
		}
	}
	return err
}

func isUnavailable(err error) bool {
	return errdefs.IsUnavailable(errdefs.FromGRPC(err))
}

func setupNetworking(ctx context.Context, task containerd.Task, c *config.Container) error {
	ip, err := cfg.Network(c.Network).Create(task)
	if err != nil {
		return err
	}
	if ip != "" {
		logrus.WithField("id", task.ID()).WithField("ip", ip).Debug("setup network interface")
		for name, srv := range c.Services {
			if err := cfg.GetRegister().Register(task.ID(), name, ip, srv); err != nil {
				logrus.WithError(err).Error("register service")
			}
		}
	}
	return nil
}

func systemdPreSetup(clix *cli.Context) error {
	id := clix.Args().First()
	if id == "" {
		return errIDRequired
	}
	return system.Ready(cfg)
}

func setupResolvConf() error {
	f, err := os.Create(filepath.Join(config.Root, "resolv.conf"))
	if err != nil {
		return err
	}
	defer f.Close()
	for _, ns := range cfg.Nameservers {
		if _, err := f.WriteString(fmt.Sprintf("nameserver %s\n", ns)); err != nil {
			return err
		}
	}
	return nil
}

func setupApparmor() error {
	return apparmor.WithDefaultProfile("boss")(nil, nil, nil, &specs.Spec{
		Process: &specs.Process{},
	})
}

func cleanupPreviousTask(id string) error {
	ctx := cfg.Context()
	container, err := cfg.Client().LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	_, err = task.Delete(ctx, containerd.WithProcessKill)
	return err
}

func getConfig(ctx context.Context, container containerd.Container) (*config.Container, error) {
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	d := info.Extensions[config.Extension]
	v, err := typeurl.UnmarshalAny(&d)
	if err != nil {
		return nil, err
	}
	return v.(*config.Container), nil
}