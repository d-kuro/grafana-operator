package config

import (
	"fmt"
	"sync"
	"time"

	"github.com/integr8ly/grafana-operator/v3/pkg/apis/integreatly/v1alpha1"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

const (
	ConfigGrafanaImage              = "grafana.image.url"
	ConfigGrafanaImageTag           = "grafana.image.tag"
	ConfigPluginsInitContainerImage = "grafana.plugins.init.container.image.url"
	ConfigPluginsInitContainerTag   = "grafana.plugins.init.container.image.tag"
	ConfigOperatorNamespace         = "grafana.operator.namespace"
	ConfigDashboardLabelSelector    = "grafana.dashboard.selector"
	ConfigOpenshift                 = "mode.openshift"
	ConfigJsonnetBasePath           = "grafonnet.location"
	GrafanaDataPath                 = "/var/lib/grafana"
	GrafanaLogsPath                 = "/var/log/grafana"
	GrafanaPluginsPath              = "/var/lib/grafana/plugins"
	GrafanaProvisioningPath         = "/etc/grafana/provisioning/"
	PluginsInitContainerImage       = "quay.io/integreatly/grafana_plugins_init"
	PluginsInitContainerTag         = "0.0.3"
	PluginsUrl                      = "https://grafana.com/api/plugins/%s/versions/%s"
	RequeueDelay                    = time.Second * 10
	SecretsMountDir                 = "/etc/grafana-secrets/"
	ConfigMapsMountDir              = "/etc/grafana-configmaps/"
	ConfigRouteWatch                = "watch.routes"
	ConfigGrafanaDashboardsSynced   = "grafana.dashboards.synced"
	JsonnetBasePath                 = "/opt/jsonnet"
)

type ControllerConfig struct {
	*sync.Mutex
	Values     map[string]interface{}
	Plugins    map[string]v1alpha1.PluginList
	Dashboards map[string][]*v1alpha1.GrafanaDashboardRef
}

var instance *ControllerConfig
var once sync.Once

var log = logf.Log.WithName("controller_config")

func GetControllerConfig() *ControllerConfig {
	once.Do(func() {
		instance = &ControllerConfig{
			Mutex:      &sync.Mutex{},
			Values:     map[string]interface{}{},
			Plugins:    map[string]v1alpha1.PluginList{},
			Dashboards: map[string][]*v1alpha1.GrafanaDashboardRef{},
		}
	})
	return instance
}

func (c *ControllerConfig) GetDashboardId(namespace, name string) string {
	return fmt.Sprintf("%v/%v", namespace, name)
}

func (c *ControllerConfig) GetAllPlugins() v1alpha1.PluginList {
	c.Lock()
	defer c.Unlock()

	var plugins v1alpha1.PluginList
	for _, v := range GetControllerConfig().Plugins {
		plugins = append(plugins, v...)
	}
	return plugins
}

func (c *ControllerConfig) GetPluginsFor(dashboard *v1alpha1.GrafanaDashboard) v1alpha1.PluginList {
	c.Lock()
	defer c.Unlock()
	return c.Plugins[c.GetDashboardId(dashboard.Namespace, dashboard.Name)]
}

func (c *ControllerConfig) SetPluginsFor(dashboard *v1alpha1.GrafanaDashboard) {
	id := c.GetDashboardId(dashboard.Namespace, dashboard.Name)
	c.Lock()
	defer c.Unlock()
	c.Plugins[id] = dashboard.Spec.Plugins
}

func (c *ControllerConfig) RemovePluginsFor(namespace, name string) {
	id := c.GetDashboardId(namespace, name)
	if _, ok := c.Plugins[id]; ok {
		delete(c.Plugins, id)
	}
}

func (c *ControllerConfig) AddDashboard(dashboard *v1alpha1.GrafanaDashboard, grafana *v1alpha1.Grafana, folderId *int64, folderName string) map[string][]*v1alpha1.GrafanaDashboardRef {
	ns := dashboard.Namespace

	dashboardRef := make(map[string][]*v1alpha1.GrafanaDashboardRef)

	if grafana.Status.InstalledDashboards != nil {
		dashboardRef = grafana.Status.InstalledDashboards
	}

	if i, exists := c.HasDashboard(grafana, ns, dashboard.Name); !exists {
		dashboardRef[ns] = append(dashboardRef[ns], &v1alpha1.GrafanaDashboardRef{
			Name:       dashboard.Name,
			Namespace:  ns,
			UID:        dashboard.UID(),
			Hash:       dashboard.Hash(),
			FolderId:   folderId,
			FolderName: dashboard.Spec.CustomFolderName,
		})
	} else {
		dashboardRef[ns][i].Namespace = ns
		dashboardRef[ns][i].UID = dashboard.UID()
		dashboardRef[ns][i].Hash = dashboard.Hash()
		dashboardRef[ns][i].FolderId = folderId
		dashboardRef[ns][i].FolderName = folderName

	}

	return dashboardRef
}

func (c *ControllerConfig) InvalidateDashboards(grafana *v1alpha1.Grafana) map[string][]*v1alpha1.GrafanaDashboardRef {
	var dashboardRefs map[string][]*v1alpha1.GrafanaDashboardRef

	if grafana.Status.InstalledDashboards != nil {
		dashboardRefs = grafana.Status.InstalledDashboards
	}

	for _, v := range dashboardRefs {
		for _, d := range v {
			d.Hash = ""
		}
	}

	return dashboardRefs
}

func (c *ControllerConfig) RemoveDashboard(grafana *v1alpha1.Grafana, namespace, name string) map[string][]*v1alpha1.GrafanaDashboardRef {
	var dashboardRefs map[string][]*v1alpha1.GrafanaDashboardRef

	if grafana.Status.InstalledDashboards != nil {
		dashboardRefs = grafana.Status.InstalledDashboards
	}

	if i, exists := c.HasDashboard(grafana, namespace, name); exists {
		list := dashboardRefs[namespace]
		list = append(list[:i], list[i+1:]...)
		dashboardRefs[namespace] = list
	}

	return dashboardRefs
}

func (c *ControllerConfig) GetDashboards(grafana *v1alpha1.Grafana, namespace string) []*v1alpha1.GrafanaDashboardRef {
	// Cluster level?
	if namespace == "" {
		var dashboards []*v1alpha1.GrafanaDashboardRef
		for _, v := range grafana.Status.InstalledDashboards {
			dashboards = append(dashboards, v...)
		}

		return dashboards
	}

	if dashboards, ok := grafana.Status.InstalledDashboards[namespace]; ok {
		return dashboards
	}

	return []*v1alpha1.GrafanaDashboardRef{}
}

func (c *ControllerConfig) AddConfigItem(key string, value interface{}) {
	c.Lock()
	defer c.Unlock()
	if key != "" && value != nil && value != "" {
		c.Values[key] = value
	}
}

func (c *ControllerConfig) RemoveConfigItem(key string) {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.Values[key]; ok {
		delete(c.Values, key)
	}
}

func (c *ControllerConfig) GetConfigItem(key string, defaultValue interface{}) interface{} {
	if c.HasConfigItem(key) {
		return c.Values[key]
	}
	return defaultValue
}

func (c *ControllerConfig) GetConfigString(key, defaultValue string) string {
	if c.HasConfigItem(key) {
		return c.Values[key].(string)
	}
	return defaultValue
}

func (c *ControllerConfig) GetConfigBool(key string, defaultValue bool) bool {
	if c.HasConfigItem(key) {
		return c.Values[key].(bool)
	}
	return defaultValue
}

func (c *ControllerConfig) GetConfigTimestamp(key string, defaultValue time.Time) time.Time {
	if c.HasConfigItem(key) {
		return c.Values[key].(time.Time)
	}
	return defaultValue
}

func (c *ControllerConfig) HasConfigItem(key string) bool {
	c.Lock()
	defer c.Unlock()
	_, ok := c.Values[key]
	return ok
}

func (c *ControllerConfig) HasDashboard(grafana *v1alpha1.Grafana, namespace, name string) (int, bool) {
	if dashboards, ok := grafana.Status.InstalledDashboards[namespace]; ok {
		for i, dashboard := range dashboards {
			if dashboard.Name == name {
				return i, true
			}
		}
	}

	return -1, false
}

func (c *ControllerConfig) Cleanup(plugins bool) {
	c.Lock()
	defer c.Unlock()
	c.Dashboards = map[string][]*v1alpha1.GrafanaDashboardRef{}

	if plugins {
		c.Plugins = map[string]v1alpha1.PluginList{}
	}
}
