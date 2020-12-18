package container

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/formatter"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type statsOptions struct {
	all        bool
	noStream   bool
	noTrunc    bool
	format     string
	containers []string
}

func GetStats(dockerCli *client.Client) ([]StatsEntry, error) {
	var opts = statsOptions{
		all:        true,
		noStream:   true,
		noTrunc:    false,
		format:     "",
		containers: nil,
	}
	showAll := len(opts.containers) == 0
	closeChan := make(chan error)
	ctx := context.Background()

	// monitorContainerEvents watches for container creation and removal (only
	// used when calling `docker stats` without arguments).
	monitorContainerEvents := func(started chan<- struct{}, c chan events.Message) {
		f := filters.NewArgs()
		f.Add("type", "container")
		options := types.EventsOptions{
			Filters: f,
		}

		eventq, errq := dockerCli.Events(ctx, options)

		// Whether we successfully subscribed to eventq or not, we can now
		// unblock the main goroutine.
		close(started)

		for {
			select {
			case event := <-eventq:
				c <- event
			case err := <-errq:
				closeChan <- err
				return
			}
		}
	}

	// Get the daemonOSType if not set already
	if daemonOSType == "" {
		svctx := context.Background()
		sv, err := dockerCli.ServerVersion(svctx)
		if err != nil {
			return nil, err
		}
		daemonOSType = sv.Os
	}

	// waitFirst is a WaitGroup to wait first stat data's reach for each container
	waitFirst := &sync.WaitGroup{}

	cStats := stats{}
	// getContainerList simulates creation event for all previously existing
	// containers (only used when calling `docker stats` without arguments).
	getContainerList := func() {
		options := types.ContainerListOptions{
			All: opts.all,
		}
		cs, err := dockerCli.ContainerList(ctx, options)
		if err != nil {
			closeChan <- err
		}
		for _, container := range cs {
			s := NewStats(container.ID[:12])
			if cStats.add(s) {
				waitFirst.Add(1)
				go collect(ctx, s, dockerCli, !opts.noStream, waitFirst)
			}
		}
	}

	if showAll {
		// If no names were specified, start a long running goroutine which
		// monitors container events. We make sure we're subscribed before
		// retrieving the list of running containers to avoid a race where we
		// would "miss" a creation.
		started := make(chan struct{})
		eh := command.InitEventHandler()
		eh.Handle("create", func(e events.Message) {
			if opts.all {
				s := NewStats(e.ID[:12])
				if cStats.add(s) {
					waitFirst.Add(1)
					go collect(ctx, s, dockerCli, !opts.noStream, waitFirst)
				}
			}
		})

		eh.Handle("start", func(e events.Message) {
			s := NewStats(e.ID[:12])
			if cStats.add(s) {
				waitFirst.Add(1)
				go collect(ctx, s, dockerCli, !opts.noStream, waitFirst)
			}
		})

		eh.Handle("die", func(e events.Message) {
			if !opts.all {
				cStats.remove(e.ID[:12])
			}
		})

		eventChan := make(chan events.Message)
		go eh.Watch(eventChan)
		go monitorContainerEvents(started, eventChan)
		defer close(eventChan)
		<-started

		// Start a short-lived goroutine to retrieve the initial list of
		// containers.
		getContainerList()

		// make sure each container get at least one valid stat data
		waitFirst.Wait()
	} else {
		// Artificially send creation events for the containers we were asked to
		// monitor (same code path than we use when monitoring all containers).
		for _, name := range opts.containers {
			s := NewStats(name)
			if cStats.add(s) {
				waitFirst.Add(1)
				go collect(ctx, s, dockerCli, !opts.noStream, waitFirst)
			}
		}

		// We don't expect any asynchronous errors: closeChan can be closed.
		close(closeChan)

		// make sure each container get at least one valid stat data
		waitFirst.Wait()

		var errs []string
		cStats.mu.Lock()
		for _, c := range cStats.cs {
			if err := c.GetError(); err != nil {
				errs = append(errs, err.Error())
			}
		}
		cStats.mu.Unlock()
		if len(errs) > 0 {
			return nil, errors.New(strings.Join(errs, "\n"))
		}
	}

	ccstats := []StatsEntry{}
	cStats.mu.Lock()
	for _, c := range cStats.cs {
		ccstats = append(ccstats, c.GetStatistics())
	}
	cStats.mu.Unlock()

	return ccstats, nil
}
