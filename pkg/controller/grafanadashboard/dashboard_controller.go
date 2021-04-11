package grafanadashboard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	grafanav1alpha1 "github.com/integr8ly/grafana-operator/v3/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/grafana-operator/v3/pkg/controller/config"
	"github.com/integr8ly/grafana-operator/v3/pkg/controller/model"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ControllerName                = "controller_grafanadashboard"
	grafanaDashboardFinalizerName = "grafanadashboard.finalizers.integreatly.org"
)

var log = logf.Log.WithName(ControllerName)

// Add creates a new GrafanaDashboard Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, namespace string) error {
	return add(mgr, newReconciler(mgr), namespace)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileGrafanaDashboard{
		client: mgr.GetClient(),
		transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		config:   config.GetControllerConfig(),
		recorder: mgr.GetEventRecorderFor(ControllerName),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler, namespace string) error {
	// Create a new controller
	c, err := controller.New("grafanadashboard-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource GrafanaDashboard
	err = c.Watch(&source.Kind{Type: &grafanav1alpha1.GrafanaDashboard{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &grafanav1alpha1.Grafana{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	log.V(1).Info("Starting dashboard controller")

	return nil
}

var _ reconcile.Reconciler = &ReconcileGrafanaDashboard{}

// ReconcileGrafanaDashboard reconciles a GrafanaDashboard object
type ReconcileGrafanaDashboard struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	transport *http.Transport
	config    *config.ControllerConfig
	recorder  record.EventRecorder
}

func (r *ReconcileGrafanaDashboard) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	ctx := context.Background()

	var grafanas grafanav1alpha1.GrafanaList

	if err := r.client.List(ctx, &grafanas); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get the list of grafana: %w", err)
	}

	var readyGrafanas []grafanav1alpha1.Grafana

	for _, g := range grafanas.Items {
		if g.Status.Ready {
			readyGrafanas = append(readyGrafanas, g)
		}
	}

	// Fetch the GrafanaDashboard instance
	var dashboard grafanav1alpha1.GrafanaDashboard
	err := r.client.Get(ctx, request.NamespacedName, &dashboard)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}

		log.Error(err, fmt.Sprintf("unable to fetch GrafanaDashboard %s/%s", request.Namespace, request.Name))

		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if dashboard.DeletionTimestamp == nil {
		if !controllerutil.ContainsFinalizer(&dashboard, grafanaDashboardFinalizerName) {
			dashboard2 := dashboard.DeepCopy()
			controllerutil.AddFinalizer(dashboard2, grafanaDashboardFinalizerName)

			if err := r.client.Update(ctx, dashboard2); err != nil {
				log.Error(err, fmt.Sprintf("failed to add finalizer to GrafanaDashboard %s/%s", request.Namespace, request.Name))

				return reconcile.Result{}, err
			}

			return reconcile.Result{Requeue: true}, nil
		}

		var wg sync.WaitGroup
		var errors []error

		for _, grafana := range readyGrafanas {
			grafana := grafana

			wg.Add(1)

			go func() {
				defer wg.Done()

				if err := r.reconcileDashboards(ctx, dashboard, grafana); err != nil {
					log.Error(err, "failed to reconcile dashboard",
						"dashboard", fmt.Sprintf("%s/%s", dashboard.Namespace, dashboard.Name),
						"grafana", fmt.Sprintf("%s/%s", grafana.Namespace, grafana.Name))

					errors = append(errors, err)
				}
			}()
		}

		wg.Wait()

		if len(errors) != 0 {
			// If error is returned, controller-runtime will requeue to the workqueue.
			return reconcile.Result{}, buildError(errors)
		}

		return reconcile.Result{}, nil
	}

	log.Info(fmt.Sprintf("start finalizing GrafanaDashboard %s/%s", request.Namespace, request.Name))

	var wg sync.WaitGroup
	var errors []error

	for _, grafana := range readyGrafanas {
		grafana := grafana

		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := r.reconcileFinalizeDashboards(ctx, dashboard, grafana); err != nil {
				log.Error(err, "failed to finalize dashboard",
					"dashboard", fmt.Sprintf("%s/%s", dashboard.Namespace, dashboard.Name),
					"grafana", fmt.Sprintf("%s/%s", grafana.Namespace, grafana.Name))
			}
		}()
	}

	wg.Wait()

	if len(errors) != 0 {
		// If error is returned, controller-runtime will requeue to the workqueue.
		return reconcile.Result{}, buildError(errors)
	}

	dashboard2 := dashboard.DeepCopy()
	controllerutil.RemoveFinalizer(dashboard2, grafanaDashboardFinalizerName)

	if err := r.client.Update(ctx, dashboard2); err != nil {
		log.Error(err, fmt.Sprintf("failed to add finalizer to GrafanaDashboard %s/%s", request.Namespace, request.Name))

		return reconcile.Result{}, err
	}

	log.Info(fmt.Sprintf("finalizing GrafanaDashboard %s/%s is completed", request.Namespace, request.Name))

	return reconcile.Result{}, nil
}

// check if the labels on a namespace match a given label selector
func (r *ReconcileGrafanaDashboard) checkNamespaceLabels(ctx context.Context,
	dashboard *grafanav1alpha1.GrafanaDashboard, grafana *grafanav1alpha1.Grafana,
) (bool, error) {
	key := client.ObjectKey{
		Name: dashboard.Namespace,
	}
	ns := &corev1.Namespace{}
	err := r.client.Get(ctx, key, ns)
	if err != nil {
		return false, err
	}
	selector, err := metav1.LabelSelectorAsSelector(grafana.Spec.DashboardNamespaceSelector)

	if err != nil {
		return false, err
	}

	return selector.Empty() || selector.Matches(labels.Set(ns.Labels)), nil
}

func (r *ReconcileGrafanaDashboard) reconcileDashboards(ctx context.Context, dashboard grafanav1alpha1.GrafanaDashboard, grafana grafanav1alpha1.Grafana) error {
	// Collect known and namespace dashboards
	var knownDashboards []*grafanav1alpha1.GrafanaDashboardRef
	if grafana.Status.InstalledDashboards != nil {
		if dashboards, ok := grafana.Status.InstalledDashboards[dashboard.Namespace]; ok {
			knownDashboards = dashboards
		}
	}

	// Returns the hash of a dashboard if it is known
	findHash := func(item *grafanav1alpha1.GrafanaDashboard) string {
		for _, d := range knownDashboards {
			if item.Name == d.Name && item.Namespace == d.Namespace {
				return d.Hash
			}
		}
		return ""
	}

	if !r.isMatch(&dashboard, &grafana) {
		log.V(1).Info(fmt.Sprintf("dashboard %v/%v found but selectors do not match",
			dashboard.Namespace, dashboard.Name))

		return nil
	}

	grafanaClient, err := r.getClient(ctx, &grafana)
	if err != nil {
		return fmt.Errorf("failed to get Grafana API client: %w", err)
	}

	folderName := dashboard.Namespace
	if dashboard.Spec.CustomFolderName != "" {
		folderName = dashboard.Spec.CustomFolderName
	}

	folder, err := grafanaClient.CreateOrUpdateFolder(folderName)
	if err != nil {
		log.Error(err, fmt.Sprintf("failed to get or create namespace folder %v for dashboard %v", folderName, dashboard.Name))
		r.manageError(&dashboard, err)

		return fmt.Errorf("failed to get or create namespace folder %s for dashboard %s: %w", folderName, dashboard.Name, err)
	}

	var folderId int64
	if folder.ID == nil {
		folderId = 0
	} else {
		folderId = *folder.ID
	}

	// Process the dashboard. Use the known hash of an existing dashboard
	// to determine if an update is required
	knownHash := findHash(&dashboard)

	pipeline := NewDashboardPipeline(r.client, &dashboard, ctx)

	processed, err := pipeline.ProcessDashboard(knownHash, &folderId, folderName)
	if err != nil {
		log.Error(err, fmt.Sprintf("cannot process dashboard %v/%v", dashboard.Namespace, dashboard.Name))
		r.manageError(&dashboard, err)

		return fmt.Errorf("cannot process dashboard %s/%s: %w", dashboard.Namespace, dashboard.Name, err)
	}

	if processed == nil {
		r.config.SetPluginsFor(&dashboard)

		return nil
	}
	// Check labels only when DashboardNamespaceSelector isnt empty
	if grafana.Spec.DashboardNamespaceSelector != nil {
		matchesNamespaceLabels, err := r.checkNamespaceLabels(ctx, &dashboard, &grafana)
		if err != nil {
			r.manageError(&dashboard, err)

			return fmt.Errorf("failed to check namespace labels: %w", err)
		}

		if matchesNamespaceLabels == false {
			log.V(1).Info(fmt.Sprintf("dashboard %v skipped because the namespace labels do not match", dashboard.Name))

			return nil
		}
	}

	_, err = grafanaClient.CreateOrUpdateDashboard(processed, folderId, folderName)
	if err != nil {
		log.Error(err, fmt.Sprintf("cannot submit dashboard %v/%v", dashboard.Namespace, dashboard.Name))
		r.manageError(&dashboard, err)

		return fmt.Errorf("cannot submit dashboard %s/%s: %w", dashboard.Namespace, dashboard.Name, err)
	}

	installedDashboard := r.config.AddDashboard(&dashboard, &grafana, &folderId, folderName)

	r.manageSuccess(ctx, &dashboard, grafana.DeepCopy(), installedDashboard)

	return nil
}

func (r *ReconcileGrafanaDashboard) reconcileFinalizeDashboards(ctx context.Context, dashboard grafanav1alpha1.GrafanaDashboard, grafana grafanav1alpha1.Grafana) error {
	i, exists := r.config.HasDashboard(&grafana, dashboard.Namespace, dashboard.Name)
	if !exists {
		return nil
	}

	deleteTarget := grafana.Status.InstalledDashboards[dashboard.Namespace][i]

	grafanaClient, err := r.getClient(ctx, &grafana)
	if err != nil {
		return fmt.Errorf("failed to get Grafana API client: %w", err)
	}

	// If status code 404 is returned from Delete dashboard API, continue.
	// This prevents the process from being interrupted when a subsequent process fails and a retry is performed.
	status, err := grafanaClient.DeleteDashboardByUID(dashboard.UID())
	if err != nil && !errors.Is(err, ErrDashboardNotFound) {
		return fmt.Errorf("error deleting dashboard %s, status %s/%s: %w",
			dashboard.UID(), *status.Status, *status.Message, err)
	}

	log.V(1).Info(fmt.Sprintf("delete result was %v", *status.Message))

	installedDashboard := r.config.RemoveDashboard(&grafana, dashboard.Namespace, dashboard.Name)

	// Check for empty managed folders (namespace-named) and delete obsolete ones
	if deleteTarget.FolderName == "" || deleteTarget.FolderName == dashboard.Namespace {
		if safe := grafanaClient.SafeToDelete(installedDashboard[dashboard.Namespace], deleteTarget.FolderId); !safe {
			log.V(1).Info("folder cannot be deleted as it's being used by other dashboards")

			return nil
		}

		// If status code 404 is returned from Delete folder API, continue.
		// This prevents the process from being interrupted when a subsequent process fails and a retry is performed.
		err := grafanaClient.DeleteFolder(deleteTarget.FolderId)
		if err != nil && !errors.Is(err, ErrFolderNotFound) {
			return fmt.Errorf("delete folder %d failed: %w", *deleteTarget.FolderId, err)
		}
	}

	grafana2 := grafana.DeepCopy()
	grafana2.Status.InstalledDashboards = installedDashboard

	if err := r.client.Status().Update(ctx, grafana2); err != nil {
		return fmt.Errorf("failed to update Grafana status %s/%s: %w", grafana.Namespace, grafana.Name, err)
	}

	return nil
}

// Handle success case: update dashboard metadata (id, uid) and update the list
// of plugins
func (r *ReconcileGrafanaDashboard) manageSuccess(ctx context.Context,
	dashboard *grafanav1alpha1.GrafanaDashboard, grafana *grafanav1alpha1.Grafana,
	installedDashboards map[string][]*grafanav1alpha1.GrafanaDashboardRef,
) {
	msg := fmt.Sprintf("dashboard %v/%v successfully submitted",
		dashboard.Namespace,
		dashboard.Name)
	r.recorder.Event(dashboard, "Normal", "Success", msg)
	log.V(1).Info(msg)
	r.config.SetPluginsFor(dashboard)

	grafana.Status.InstalledDashboards = installedDashboards

	err := r.client.Status().Update(ctx, grafana)
	if err != nil {
		log.Error(err, fmt.Sprintf("failed to update Grafana status %s/%s", grafana.Namespace, grafana.Name))
	}
}

// Handle error case: update dashboard with error message and status
func (r *ReconcileGrafanaDashboard) manageError(dashboard *grafanav1alpha1.GrafanaDashboard, issue error) {
	r.recorder.Event(dashboard, "Warning", "ProcessingError", issue.Error())

	// Ignore conclicts. Resource might just be outdated.
	if k8serrors.IsConflict(issue) {
		return
	}

	log.Error(issue, "error updating dashboard")
}

// Get an authenticated grafana API client
func (r *ReconcileGrafanaDashboard) getClient(ctx context.Context, grafana *grafanav1alpha1.Grafana) (GrafanaClient, error) {
	if grafana.Status.AdminURL == nil || *grafana.Status.AdminURL == "" {
		return nil, errors.New("cannot get grafana admin url")
	}

	url := *grafana.Status.AdminURL

	if grafana.Status.AdminUser == nil || grafana.Status.AdminPassword == nil {
		return nil, errors.New("cannot get grafana admin secret")
	}

	var adminUserSecret corev1.Secret
	err := r.client.Get(ctx,
		types.NamespacedName{Namespace: grafana.Namespace, Name: grafana.Status.AdminUser.SecretName},
		&adminUserSecret)
	if err != nil {
		return nil, err
	}

	username, ok := adminUserSecret.Data[grafana.Status.AdminUser.Key]
	if !ok {
		return nil, errors.New("invalid credentials (username)")
	}

	var adminPasswordSecret corev1.Secret
	err = r.client.Get(ctx,
		types.NamespacedName{Namespace: grafana.Namespace, Name: grafana.Status.AdminPassword.SecretName},
		&adminPasswordSecret)
	if err != nil {
		return nil, err
	}

	password, ok := adminUserSecret.Data[grafana.Status.AdminPassword.Key]
	if !ok {
		return nil, errors.New("invalid credentials (password)")
	}

	var seconds int
	if grafana.Spec.Client != nil && grafana.Spec.Client.TimeoutSeconds != nil {
		seconds = *grafana.Spec.Client.TimeoutSeconds
		if seconds <= 0 {
			seconds = model.GrafanaDefaultClientTimeoutSeconds
		}
	} else {
		seconds = model.GrafanaDefaultClientTimeoutSeconds
	}

	return NewGrafanaClient(url, string(username), string(password), r.transport, time.Duration(seconds)), nil
}

// Test if a given dashboard matches an array of label selectors
func (ReconcileGrafanaDashboard) isMatch(item *grafanav1alpha1.GrafanaDashboard, grafana *grafanav1alpha1.Grafana) bool {
	if grafana.Spec.DashboardLabelSelector == nil {
		return false
	}

	match, err := item.MatchesSelectors(grafana.Spec.DashboardLabelSelector)
	if err != nil {
		log.Error(err, fmt.Sprintf("error matching selectors against %v/%v",
			item.Namespace,
			item.Name))
		return false
	}
	return match
}

func buildError(errors []error) error {
	var errBuilder strings.Builder
	first := true

	for _, err := range errors {
		if first {
			first = false
		} else {
			errBuilder.WriteString("; ")
		}

		errBuilder.WriteString(err.Error())
	}

	return fmt.Errorf(errBuilder.String())
}
