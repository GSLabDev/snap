package control

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"strings"

	"github.com/intelsdi-x/gomit"

	"github.com/intelsdi-x/pulse/control/plugin"
	"github.com/intelsdi-x/pulse/core/control_event"
	"github.com/intelsdi-x/pulse/pkg/logger"
)

const (
	HandlerRegistrationName = "control.runner"

	// availablePlugin States
	PluginRunning availablePluginState = iota - 1 // Default value (0) is Running
	PluginStopped
	PluginDisabled

	// Until more advanced decisioning on starting exists this is the max number to spawn.
	MaximumRunningPlugins = 3
)

// TBD
type executablePlugin interface {
	Start() error
	Kill() error
	WaitForResponse(time.Duration) (*plugin.Response, error)
}

type idCounter struct {
	id    int
	mutex *sync.Mutex
}

func (i *idCounter) Next() int {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.id++
	return i.id
}

// Handles events pertaining to plugins and control the runnning state accordingly.
type runner struct {
	delegates        []gomit.Delegator
	emitter          gomit.Emitter
	monitor          *monitor
	availablePlugins *availablePlugins
	metricCatalog    catalogsMetrics
	pluginManager    managesPlugins
	mutex            *sync.Mutex
	apIdCounter      *idCounter
}

func newRunner() *runner {
	r := &runner{
		monitor:          newMonitor(),
		availablePlugins: newAvailablePlugins(),
		mutex:            &sync.Mutex{},
		apIdCounter:      &idCounter{mutex: &sync.Mutex{}},
	}
	return r
}

func (r *runner) SetMetricCatalog(c catalogsMetrics) {
	r.metricCatalog = c
}

func (r *runner) SetEmitter(e gomit.Emitter) {
	r.emitter = e
}

func (r *runner) SetPluginManager(m managesPlugins) {
	r.pluginManager = m
}

func (r *runner) AvailablePlugins() *availablePlugins {
	return r.availablePlugins
}

func (r *runner) Monitor() *monitor {
	return r.monitor
}

// Adds Delegates (gomit.Delegator) for adding Runner handlers to on Start and
// unregistration on Stop.
func (r *runner) AddDelegates(delegates ...gomit.Delegator) {
	// Append the variadic collection of gomit.RegisterHanlders to r.delegates
	r.delegates = append(r.delegates, delegates...)
}

// Begin handing events and managing available plugins
func (r *runner) Start() error {
	// Delegates must be added before starting if none exist
	// then this Runner can do nothing and should not start.
	if len(r.delegates) == 0 {
		return errors.New("No delegates added before called Start()")
	}

	// For each delegate register needed handlers
	for _, del := range r.delegates {
		e := del.RegisterHandler(HandlerRegistrationName, r)
		if e != nil {
			return e
		}
	}

	// Start the monitor
	r.monitor.Start(r.availablePlugins)

	logger.Debug("runner.start", "started")
	return nil
}

// Stop handling, gracefully stop all plugins.
func (r *runner) Stop() []error {
	var errs []error

	// Stop the monitor
	r.monitor.Stop()

	// TODO: Actually stop the plugins

	// For each delegate unregister needed handlers
	for _, del := range r.delegates {
		e := del.UnregisterHandler(HandlerRegistrationName)
		if e != nil {
			errs = append(errs, e)
		}
	}
	defer logger.Debug("runner.stop", "stopped")
	return errs
}

func (r *runner) startPlugin(p executablePlugin) (*availablePlugin, error) {
	e := p.Start()
	if e != nil {
		e_ := errors.New("error while starting plugin: " + e.Error())
		defer logger.Error("runner.startplugin", e_.Error())
		return nil, e_
	}

	// Wait for plugin response
	resp, err := p.WaitForResponse(time.Second * 3)
	if err != nil {
		return nil, errors.New("error while waiting for response: " + err.Error())
	}

	if resp == nil {
		return nil, errors.New("no reponse object returned from plugin")
	}

	if resp.State != plugin.PluginSuccess {
		return nil, errors.New("plugin could not start error: " + resp.ErrorMessage)
	}

	// build availablePlugin
	ap, err := newAvailablePlugin(resp, r.apIdCounter.Next(), r.emitter)
	if err != nil {
		return nil, err
	}

	// Ping through client
	err = ap.Client.Ping()
	if err != nil {
		return nil, err
	}

	r.availablePlugins.Insert(ap)
	logger.Infof("runner.events", "available plugin started (%s)", ap.String())

	return ap, nil
}

func (r *runner) stopPlugin(reason string, ap *availablePlugin) error {
	err := ap.Stop(reason)
	if err != nil {
		return err
	}
	err = r.availablePlugins.Remove(ap)
	if err != nil {
		return err
	}
	return nil
}

// Empty handler acting as placeholder until implementation. This helps tests
// pass to ensure registration works.
func (r *runner) HandleGomitEvent(e gomit.Event) {

	switch v := e.Body.(type) {
	case *control_event.PublisherSubscriptionEvent:
		r.mutex.Lock()
		defer r.mutex.Unlock()
		logger.Debugf("runner.events", "handling publisher subscription event (%v:v%v)", v.PluginName, v.PluginVersion)

		for r.pluginManager.LoadedPlugins().Next() {
			_, lp := r.pluginManager.LoadedPlugins().Item()
			logger.Debugf("runner.events", "subscription request name: %v version: %v", v.PluginName, v.PluginVersion)
			logger.Debugf("runner.events", "loaded plugin name: %v version: %v type: %v", lp.Name(), lp.Version(), lp.TypeName())
			if lp.TypeName() == "publisher" && lp.Name() == v.PluginName && lp.Version() == v.PluginVersion {
				pool := r.availablePlugins.Publishers.GetPluginPool(lp.Key())
				ok := checkPool(pool, lp.Key())
				if !ok {
					return
				}

				ePlugin, err := plugin.NewExecutablePlugin(r.pluginManager.GenerateArgs(lp.Path), lp.Path)
				_, err = r.startPlugin(ePlugin)
				if err != nil {
					fmt.Println(err)
					panic(err)
				}
			}

		}
	case *control_event.MetricSubscriptionEvent:
		r.mutex.Lock()
		defer r.mutex.Unlock()
		logger.Debugf("runner.events", "handling metric subscription event (%s v%d)", strings.Join(v.MetricNamespace, "/"), v.Version)

		// Our logic here is simple for alpha. We should replace with parameter managed logic.
		//
		// 1. Get the loaded plugin for the subscription.
		// 2. Check that at least one available plugin of that type is running
		// 3. If not start one

		mt, err := r.metricCatalog.Get(v.MetricNamespace, v.Version)
		if err != nil {
			// log this error # TODO with logging
			fmt.Println(err)
			return
		}
		logger.Debugf("runner.events", "plugin is (%s) for (%s v%d)", mt.Plugin.Key(), strings.Join(v.MetricNamespace, "/"), v.Version)

		pool := r.availablePlugins.Collectors.GetPluginPool(mt.Plugin.Key())
		ok := checkPool(pool, mt.Plugin.Key())
		if !ok {
			return
		}

		ePlugin, err := plugin.NewExecutablePlugin(r.pluginManager.GenerateArgs(mt.Plugin.Path), mt.Plugin.Path)
		if err != nil {
			logger.Debugf("runner:HandleGomitEvent", "Plugin %v (ver %v) error: %v", mt.Plugin.Name(), mt.Plugin.Version(), err)
			fmt.Println(err)
		}
		_, err = r.startPlugin(ePlugin)
		if err != nil {
			logger.Debugf("runner:HandleGomitEvent", "Plugin %v (ver %v) start error: %v", mt.Plugin.Name(), mt.Plugin.Version(), err)
			panic(err)
		}
	}
}

func checkPool(pool *availablePluginPool, key string) bool {
	if pool != nil && pool.Count() >= MaximumRunningPlugins {
		logger.Debugf("runner.events", "(%s) has %d available plugin running (need %d)", key, pool.Count(), MaximumRunningPlugins)
		return false
	}
	if pool == nil {
		logger.Debugf("runner.events", "not enough available plugins (%d) running for (%s) need %d", 0, key, MaximumRunningPlugins)
	} else {
		logger.Debugf("runner.events", "not enough available plugins (%d) running for (%s) need %d", pool.Count(), key, MaximumRunningPlugins)
	}
	return true
}
