package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/karimra/gnmic/config"
	"github.com/karimra/gnmic/formatters"
	"github.com/karimra/gnmic/types"
	"github.com/manifoldco/promptui"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/grpctunnel/tunnel"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	initLockerRetryTimer = 1 * time.Second
)

func (a *App) SubscribePreRunE(cmd *cobra.Command, args []string) error {
	a.Config.SetLocalFlagsFromFile(cmd)
	a.createCollectorDialOpts()
	return nil
}

func (a *App) SubscribeRunE(cmd *cobra.Command, args []string) error {
	defer a.InitSubscribeFlags(cmd)

	// prompt mode
	if a.PromptMode {
		return a.SubscribeRunPrompt(cmd, args)
	}
	//
	subCfg, err := a.Config.GetSubscriptions(cmd)
	if err != nil {
		return fmt.Errorf("failed reading subscriptions config: %v", err)
	}
	if len(subCfg) == 0 {
		return errors.New("no subscriptions configuration found")
	}
	// only once mode subscriptions requested
	if allSubscriptionsModeOnce(subCfg) {
		return a.SubscribeRunONCE(cmd, args, subCfg)
	}
	// only poll mode subscriptions requested
	if allSubscriptionsModePoll(subCfg) {
		return a.SubscribeRunPoll(cmd, args, subCfg)
	}
	// stream subscriptions
	err = a.initTunnelServer(tunnel.ServerConfig{
		AddTargetHandler:    a.tunServerAddTargetSubscribeHandler,
		DeleteTargetHandler: a.tunServerDeleteTargetHandler,
		RegisterHandler:     a.tunServerRegisterHandler,
		Handler:             a.tunServerHandler,
	})
	if err != nil {
		return err
	}
	_, err = a.Config.GetTargets()
	if errors.Is(err, config.ErrNoTargetsFound) {
		if !a.Config.LocalFlags.SubscribeWatchConfig &&
			len(a.Config.FileConfig.GetStringMap("loader")) == 0 &&
			!a.Config.UseTunnelServer {
			return fmt.Errorf("failed reading targets config: %v", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed reading targets config: %v", err)
	}
	err = a.readConfigs()
	if err != nil {
		return err
	}
	if a.Config.APIServer != nil && a.Config.APIServer.EnableMetrics {
		a.reg = prometheus.NewRegistry()
	}
	err = a.Config.GetClustering()
	if err != nil {
		return err
	}
	err = a.Config.GetGNMIServer()
	if err != nil {
		return err
	}
	err = a.Config.GetAPIServer()
	if err != nil {
		return err
	}
	err = a.Config.GetLoader()
	if err != nil {
		return err
	}
	//
	for {
		err := a.InitLocker()
		if err != nil {
			a.Logger.Printf("failed to init locker: %v", err)
			time.Sleep(initLockerRetryTimer)
			continue
		}
		break
	}

	a.startAPIServer()
	a.startGnmiServer()
	go a.startCluster()
	a.startIO()

	if a.Config.LocalFlags.SubscribeWatchConfig {
		go a.watchConfig()
	}

	for range a.ctx.Done() {
		return a.ctx.Err()
	}
	return nil
}

//
func (a *App) subscribeStream(ctx context.Context, name string) {
	defer a.wg.Done()
	a.TargetSubscribeStream(ctx, name)
}

func (a *App) subscribeOnce(ctx context.Context, name string) {
	defer a.wg.Done()
	err := a.TargetSubscribeOnce(ctx, name)
	if err != nil {
		a.logError(err)
	}
}

func (a *App) subscribePoll(ctx context.Context, name string) {
	defer a.wg.Done()
	a.TargetSubscribePoll(ctx, name)
}

// InitSubscribeFlags used to init or reset subscribeCmd flags for gnmic-prompt mode
func (a *App) InitSubscribeFlags(cmd *cobra.Command) {
	cmd.ResetFlags()

	cmd.Flags().StringVarP(&a.Config.LocalFlags.SubscribePrefix, "prefix", "", "", "subscribe request prefix")
	cmd.Flags().StringArrayVarP(&a.Config.LocalFlags.SubscribePath, "path", "", []string{}, "subscribe request paths")
	//cmd.MarkFlagRequired("path")
	cmd.Flags().Uint32VarP(&a.Config.LocalFlags.SubscribeQos, "qos", "q", 0, "qos marking")
	cmd.Flags().BoolVarP(&a.Config.LocalFlags.SubscribeUpdatesOnly, "updates-only", "", false, "only updates to current state should be sent")
	cmd.Flags().StringVarP(&a.Config.LocalFlags.SubscribeMode, "mode", "", "stream", "one of: once, stream, poll")
	cmd.Flags().StringVarP(&a.Config.LocalFlags.SubscribeStreamMode, "stream-mode", "", "target-defined", "one of: on-change, sample, target-defined")
	cmd.Flags().DurationVarP(&a.Config.LocalFlags.SubscribeSampleInterval, "sample-interval", "i", 0,
		"sample interval as a decimal number and a suffix unit, such as \"10s\" or \"1m30s\"")
	cmd.Flags().BoolVarP(&a.Config.LocalFlags.SubscribeSuppressRedundant, "suppress-redundant", "", false, "suppress redundant update if the subscribed value didn't not change")
	cmd.Flags().DurationVarP(&a.Config.LocalFlags.SubscribeHeartbearInterval, "heartbeat-interval", "", 0, "heartbeat interval in case suppress-redundant is enabled")
	cmd.Flags().StringSliceVarP(&a.Config.LocalFlags.SubscribeModel, "model", "", []string{}, "subscribe request used model(s)")
	cmd.Flags().BoolVar(&a.Config.LocalFlags.SubscribeQuiet, "quiet", false, "suppress stdout printing")
	cmd.Flags().StringVarP(&a.Config.LocalFlags.SubscribeTarget, "target", "", "", "subscribe request target")
	cmd.Flags().BoolVarP(&a.Config.LocalFlags.SubscribeSetTarget, "set-target", "", false, "set target name in gNMI Path prefix")
	cmd.Flags().StringSliceVarP(&a.Config.LocalFlags.SubscribeName, "name", "n", []string{}, "reference subscriptions by name, must be defined in gnmic config file")
	cmd.Flags().StringSliceVarP(&a.Config.LocalFlags.SubscribeOutput, "output", "", []string{}, "reference to output groups by name, must be defined in gnmic config file")
	cmd.Flags().BoolVarP(&a.Config.LocalFlags.SubscribeWatchConfig, "watch-config", "", false, "watch configuration changes, add or delete subscribe targets accordingly")
	cmd.Flags().DurationVarP(&a.Config.LocalFlags.SubscribeBackoff, "backoff", "", 0, "backoff time between subscribe requests")
	cmd.Flags().DurationVarP(&a.Config.LocalFlags.SubscribeLockRetry, "lock-retry", "", 5*time.Second, "time to wait between target lock attempts")
	//
	cmd.LocalFlags().VisitAll(func(flag *pflag.Flag) {
		a.Config.FileConfig.BindPFlag(fmt.Sprintf("%s-%s", cmd.Name(), flag.Name), flag)
	})
}

func (a *App) readConfigs() error {
	var err error
	_, err = a.Config.GetOutputs()
	if err != nil {
		return fmt.Errorf("failed reading outputs config: %v", err)
	}
	_, err = a.Config.GetInputs()
	if err != nil {
		return fmt.Errorf("failed reading inputs config: %v", err)
	}
	_, err = a.Config.GetActions()
	if err != nil {
		return fmt.Errorf("failed reading actions config: %v", err)
	}
	_, err = a.Config.GetEventProcessors()
	if err != nil {
		return fmt.Errorf("failed reading event processors config: %v", err)
	}
	_, err = a.LoadProtoFiles()
	if err != nil {
		return fmt.Errorf("failed loading proto files: %v", err)
	}
	return nil
}

func (a *App) handlePolledSubscriptions() {
	polledTargetsSubscriptions := a.PolledSubscriptionsTargets()
	if len(polledTargetsSubscriptions) > 0 {
		pollTargets := make([]string, 0, len(polledTargetsSubscriptions))
		for t := range polledTargetsSubscriptions {
			pollTargets = append(pollTargets, t)
		}
		sort.Slice(pollTargets, func(i, j int) bool {
			return pollTargets[i] < pollTargets[j]
		})
		s := promptui.Select{
			Label:        "select target to poll",
			Items:        pollTargets,
			HideSelected: true,
		}
		waitChan := make(chan struct{}, 1)
		waitChan <- struct{}{}
		mo := &formatters.MarshalOptions{
			Multiline: true,
			Indent:    "  ",
			Format:    a.Config.Format,
		}

		for {
			select {
			case <-waitChan:
				_, name, err := s.Run()
				if err != nil {
					fmt.Printf("failed selecting target to poll: %v\n", err)
					continue
				}
				ss := promptui.Select{
					Label:        "select subscription to poll",
					Items:        polledTargetsSubscriptions[name],
					HideSelected: true,
				}
				_, subName, err := ss.Run()
				if err != nil {
					fmt.Printf("failed selecting subscription to poll: %v\n", err)
					continue
				}
				response, err := a.clientSubscribePoll(name, subName)
				if err != nil && err != io.EOF {
					fmt.Printf("target '%s', subscription '%s': poll response error:%v\n", name, subName, err)
					continue
				}
				if response == nil {
					fmt.Printf("received empty response from target '%s'\n", name)
					continue
				}
				switch rsp := response.Response.(type) {
				case *gnmi.SubscribeResponse_SyncResponse:
					fmt.Printf("received sync response '%t' from '%s'\n", rsp.SyncResponse, name)
					waitChan <- struct{}{}
					continue
				}
				b, err := mo.Marshal(response, nil)
				if err != nil {
					fmt.Printf("target '%s', subscription '%s': poll response formatting error:%v\n", name, subName, err)
					fmt.Println(string(b))
					waitChan <- struct{}{}
					continue
				}
				fmt.Println(string(b))
				waitChan <- struct{}{}
			case <-a.ctx.Done():
				return
			}
		}

	}
}

func (a *App) startIO() {
	go a.StartCollector(a.ctx)
	a.InitOutputs(a.ctx)
	a.InitInputs(a.ctx)

	if !a.inCluster() {
		go a.startLoader(a.ctx)
		var limiter *time.Ticker
		if a.Config.LocalFlags.SubscribeBackoff > 0 {
			limiter = time.NewTicker(a.Config.LocalFlags.SubscribeBackoff)
		}

		a.wg.Add(len(a.Config.Targets))
		for name := range a.Config.Targets {
			// check if target is a tunnel discovered target.
			// in which case, do not (re)subscribe
			a.ttm.RLock()
			_, ok := a.tunTargets[name]
			a.ttm.RUnlock()
			if ok {
				continue
			}
			go a.subscribeStream(a.ctx, name)
			if limiter != nil {
				<-limiter.C
			}
		}
		if limiter != nil {
			limiter.Stop()
		}
		a.wg.Wait()
	}
}

func allSubscriptionsModeOnce(sc map[string]*types.SubscriptionConfig) bool {
	for _, sub := range sc {
		if strings.ToUpper(sub.Mode) != "ONCE" {
			return false
		}
	}
	return true
}

func allSubscriptionsModePoll(sc map[string]*types.SubscriptionConfig) bool {
	for _, sub := range sc {
		if strings.ToUpper(sub.Mode) != "POLL" {
			return false
		}
	}
	return true
}
