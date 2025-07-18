/*
Copyright 2021 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tektonconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"time"

	security "github.com/openshift/client-go/security/clientset/versioned"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	clientset "github.com/tektoncd/operator/pkg/client/clientset/versioned"
	"github.com/tektoncd/operator/pkg/common"
	reconcilerCommon "github.com/tektoncd/operator/pkg/reconciler/common"
	"github.com/tektoncd/operator/pkg/reconciler/openshift"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	nsV1 "k8s.io/client-go/informers/core/v1"
	rbacV1 "k8s.io/client-go/informers/rbac/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"knative.dev/pkg/logging"
)

const (
	pipelinesSCCRole        = "pipelines-scc-role"
	pipelinesSCCClusterRole = "pipelines-scc-clusterrole"
	pipelinesSCCRoleBinding = "pipelines-scc-rolebinding"
	pipelineSA              = "pipeline"
	PipelineRoleBinding     = "openshift-pipelines-edit"

	// TODO: Remove this after v0.55.0 release, by following a depreciation notice
	// --------------------
	pipelineRoleBindingOld  = "edit"
	rbacInstallerSetNameOld = "rbac-resources"
	// --------------------
	serviceCABundleConfigMap    = "config-service-cabundle"
	trustedCABundleConfigMap    = "config-trusted-cabundle"
	clusterInterceptors         = "openshift-pipelines-clusterinterceptors"
	namespaceVersionLabel       = "openshift-pipelines.tekton.dev/namespace-reconcile-version"
	namespaceTrustedConfigLabel = "openshift-pipelines.tekton.dev/namespace-trusted-configmaps-version"
	createdByValue              = "RBAC"
	componentNameRBAC           = "rhosp-rbac"
	rbacInstallerSetType        = "rhosp-rbac"
	rbacInstallerSetNamePrefix  = "rhosp-rbac-"
	rbacParamName               = "createRbacResource"
	trustedCABundleParamName    = "createCABundleConfigMaps"
	legacyPipelineRbacParamName = "legacyPipelineRbac"
	legacyPipelineRbac          = "true"
	serviceAccountCreationLabel = "openshift-pipelines.tekton.dev/sa-created"
)

var (
	rbacInstallerSetSelector = metav1.LabelSelector{
		MatchLabels: map[string]string{
			v1alpha1.CreatedByKey:     createdByValue,
			v1alpha1.InstallerSetType: componentNameRBAC,
		},
	}
)

// Namespace Regex to ignore the namespace for creating rbac resources.
var nsRegex = regexp.MustCompile(reconcilerCommon.NamespaceIgnorePattern)

type rbac struct {
	kubeClientSet     kubernetes.Interface
	operatorClientSet clientset.Interface
	securityClientSet security.Interface
	rbacInformer      rbacV1.ClusterRoleBindingInformer
	nsInformer        nsV1.NamespaceInformer
	ownerRef          metav1.OwnerReference
	version           string
	tektonConfig      *v1alpha1.TektonConfig
}

type NamespaceServiceAccount struct {
	ServiceAccount *corev1.ServiceAccount
	Namespace      corev1.Namespace
}

// NamespacesToReconcile holds the namespaces that need reconciliation for different features
type NamespacesToReconcile struct {
	RBACNamespaces []corev1.Namespace
	CANamespaces   []corev1.Namespace
}

func (r *rbac) cleanUp(ctx context.Context) error {

	// fetch the list of all namespaces which have label
	// `openshift-pipelines.tekton.dev/namespace-reconcile-version: <release-version>`
	labelSelector := fmt.Sprintf("%s = %s", namespaceVersionLabel, r.version)
	namespaces, err := r.kubeClientSet.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to retreive namespaces with labelSeleclector %s: %v", labelSelector, err)
	}
	// loop on namespaces and remove label if exist
	for _, n := range namespaces.Items {
		labels := n.GetLabels()
		delete(labels, namespaceVersionLabel)
		n.SetLabels(labels)
		if _, err := r.kubeClientSet.CoreV1().Namespaces().Update(ctx, &n, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update namespace %s: %v", n.Name, err)
		}
	}
	return nil
}

func (r *rbac) EnsureRBACInstallerSet(ctx context.Context) (*v1alpha1.TektonInstallerSet, error) {
	if err := r.removeObsoleteRBACInstallerSet(ctx); err != nil {
		return nil, err
	}

	rbacISet, err := checkIfInstallerSetExist(ctx, r.operatorClientSet, r.version, r.tektonConfig)
	if err != nil {
		return nil, err
	}

	if rbacISet != nil {
		return rbacISet, nil
	}
	// A new installer needs to be created
	// either because of operator version upgrade or installerSet gone missing;
	// therefore all relevant namespaces need to be reconciled for RBAC resources.
	// Hence, remove the necessary labels to ensure that the namespaces will be 'not skipped'
	// RBAC reconcile logic
	err = r.cleanUp(ctx)
	if err != nil {
		return nil, err
	}

	err = createInstallerSet(ctx, r.operatorClientSet, r.tektonConfig, r.version)
	if err != nil {
		return nil, err
	}
	return nil, v1alpha1.RECONCILE_AGAIN_ERR
}

func (r *rbac) setDefault() {
	var rbacParamFound, legacyParamFound, caBundleParamFound bool
	var createRbacResourceValue string

	for i, v := range r.tektonConfig.Spec.Params {
		if v.Name == rbacParamName {
			rbacParamFound = true
			createRbacResourceValue = v.Value
			if v.Value != "false" && v.Value != "true" {
				r.tektonConfig.Spec.Params[i].Value = "true"
			}
		}
		if v.Name == legacyPipelineRbacParamName {
			legacyParamFound = true
			if v.Value != "false" && v.Value != "true" {
				r.tektonConfig.Spec.Params[i].Value = "true"
			}
		}
		if v.Name == trustedCABundleParamName {
			caBundleParamFound = true
			if v.Value != "false" && v.Value != "true" {
				r.tektonConfig.Spec.Params[i].Value = "true"
			}
		}
	}
	if !rbacParamFound {
		r.tektonConfig.Spec.Params = append(r.tektonConfig.Spec.Params, v1alpha1.Param{
			Name:  rbacParamName,
			Value: "true",
		})
	}
	if !legacyParamFound {
		r.tektonConfig.Spec.Params = append(r.tektonConfig.Spec.Params, v1alpha1.Param{
			Name:  legacyPipelineRbacParamName,
			Value: "true",
		})
	}

	// TODO: Remove this upgrade workaround after version 1.22.
	// This logic is only needed to preserve backward compatibility for users upgrading to 1.21
	// who had createRbacResource=false and no createCABundleConfigMaps param set.
	if !caBundleParamFound {
		defaultVal := "true"
		if rbacParamFound && createRbacResourceValue == "false" {
			defaultVal = "false"
		}
		r.tektonConfig.Spec.Params = append(r.tektonConfig.Spec.Params,
			v1alpha1.Param{Name: trustedCABundleParamName, Value: defaultVal},
		)
	}
}

// ensurePreRequisites validates the resources before creation
func (r *rbac) ensurePreRequisites(ctx context.Context) error {
	logger := logging.FromContext(ctx)

	rbacISet, err := r.EnsureRBACInstallerSet(ctx)
	if err != nil {
		return err
	}
	r.ownerRef = configOwnerRef(*rbacISet)

	// make sure default SCC is in place
	defaultSCC := r.tektonConfig.Spec.Platforms.OpenShift.SCC.Default
	if defaultSCC == "" {
		// Should not really happen due to defaulting, but okay...
		return fmt.Errorf("tektonConfig.Spec.Platforms.OpenShift.SCC.Default cannot be empty")
	}
	logger.Infof("default SCC set to: %s", defaultSCC)
	if err := common.VerifySCCExists(ctx, defaultSCC, r.securityClientSet); err != nil {
		return fmt.Errorf("failed to verify scc %s exists, %w", defaultSCC, err)
	}

	prioritizedSCCList, err := common.GetSCCRestrictiveList(ctx, r.securityClientSet)
	if err != nil {
		return err
	}

	// validate maxAllowed SCC
	maxAllowedSCC := r.tektonConfig.Spec.Platforms.OpenShift.SCC.MaxAllowed
	if maxAllowedSCC != "" {
		if err := common.VerifySCCExists(ctx, maxAllowedSCC, r.securityClientSet); err != nil {
			return fmt.Errorf("failed to verify scc %s exists, %w", maxAllowedSCC, err)
		}

		isPriority, err := common.SCCAMoreRestrictiveThanB(prioritizedSCCList, defaultSCC, maxAllowedSCC)
		if err != nil {
			return err
		}
		logger.Infof("Is maxAllowed SCC: %s less restrictive than default SCC: %s? %t", maxAllowedSCC, defaultSCC, isPriority)
		if !isPriority {
			return fmt.Errorf("maxAllowed SCC: %s must be less restrictive than the default SCC: %s", maxAllowedSCC, defaultSCC)
		}
		logger.Infof("maxAllowed SCC set to: %s", maxAllowedSCC)
	} else {
		logger.Info("No maxAllowed SCC set in TektonConfig")
	}

	// Maintaining a separate cluster role for the scc declaration.
	// to assist us in managing this the scc association in a
	// granular way.
	// We need to make sure the pipelines-scc-clusterrole is up-to-date
	// irrespective of the fact that we get reconcilable namespaces or not.
	if err := r.ensurePipelinesSCClusterRole(ctx); err != nil {
		return err
	}

	return nil
}

func (r *rbac) getNamespacesToBeReconciled(ctx context.Context) (*NamespacesToReconcile, error) {
	logger := logging.FromContext(ctx)

	// fetch the list of all namespaces
	allNamespaces, err := r.kubeClientSet.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	result := &NamespacesToReconcile{
		RBACNamespaces: []corev1.Namespace{},
		CANamespaces:   []corev1.Namespace{},
	}

	for _, ns := range allNamespaces.Items {
		// ignore namespaces with name passing regex `^(openshift|kube)-`
		if ignore := nsRegex.MatchString(ns.GetName()); ignore {
			logger.Debugf("Ignoring system namespace: %s", ns.GetName())
			continue
		}

		// ignore namespaces with DeletionTimestamp set
		if ns.GetObjectMeta().GetDeletionTimestamp() != nil {
			logger.Debugf("Ignoring namespace being deleted: %s", ns.GetName())
			continue
		}

		// Check if namespace needs RBAC reconciliation
		needsRBAC := false
		// We want to monitor namespaces with the SCC annotation set
		if ns.Annotations[openshift.NamespaceSCCAnnotation] != "" {
			needsRBAC = true
		}
		// Then we want to accept namespaces that have not been reconciled yet
		if ns.Labels[namespaceVersionLabel] != r.version {
			needsRBAC = true
		} else {
			// Now we're left with namespaces that have already been reconciled.
			// We must make sure that the default SCC is in force via the ClusterRole.
			sccRoleBinding, err := r.kubeClientSet.RbacV1().RoleBindings(ns.Name).Get(ctx, pipelinesSCCRoleBinding, metav1.GetOptions{})
			if err != nil {
				// Reconcile a namespace again with missing RoleBinding
				if errors.IsNotFound(err) {
					logger.Debugf("could not find roleBinding %s in namespace %s", pipelinesSCCRoleBinding, ns.Name)
					needsRBAC = true
				} else {
					return nil, fmt.Errorf("error fetching rolebinding %s from namespace %s: %w", pipelinesSCCRoleBinding, ns.Name, err)
				}
			} else if sccRoleBinding.RoleRef.Kind != "ClusterRole" {
				logger.Infof("RoleBinding %s in namespace: %s should have CluterRole with default SCC, will reconcile again...", pipelinesSCCRoleBinding, ns.Name)
				needsRBAC = true
			}
		}

		if needsRBAC {
			logger.Debugf("Adding namespace for RBAC reconciliation: %s", ns.GetName())
			result.RBACNamespaces = append(result.RBACNamespaces, ns)
		}

		// Check if namespace needs CA bundle reconciliation
		if ns.Labels[namespaceTrustedConfigLabel] != r.version {
			logger.Debugf("Adding namespace for CA bundle reconciliation: %s", ns.GetName())
			result.CANamespaces = append(result.CANamespaces, ns)
		}
	}

	return result, nil
}

func (r *rbac) getSCCRoleInNamespace(ns *corev1.Namespace) *rbacv1.RoleRef {
	nsAnnotations := ns.GetAnnotations()
	nsSCC := nsAnnotations[openshift.NamespaceSCCAnnotation]
	// If SCC is requested by namespace annotation, then we need a Role
	if nsSCC != "" {
		return &rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     pipelinesSCCRole,
		}
	}
	// If no SCC annotation is present in the namespace, we will use the
	// pipelines-scc-clusterrole
	return &rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "ClusterRole",
		Name:     pipelinesSCCClusterRole,
	}
}

func (r *rbac) handleSCCInNamespace(ctx context.Context, ns *corev1.Namespace) error {
	logger := logging.FromContext(ctx)

	nsName := ns.GetName()
	nsAnnotations := ns.GetAnnotations()
	nsSCC := nsAnnotations[openshift.NamespaceSCCAnnotation]

	// No SCC is requested in the namespace
	if nsSCC == "" {
		// If we don't have a namespace annotation, then we don't need a
		// Role in this namespace as we will bind to the ClusterRole.
		// This happens in cases when the SCC annotation was removed from
		// the namespace.
		_, err := r.kubeClientSet.RbacV1().Roles(nsName).Get(ctx, pipelinesSCCRole, metav1.GetOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}

		// If `err == nil` AND role was found, it means that role exists
		if !errors.IsNotFound(err) {
			logger.Infof("Found leftover role: %s in namespace: %s, deleting...", pipelinesSCCRole, nsName)
			err := r.kubeClientSet.RbacV1().Roles(nsName).Delete(ctx, pipelinesSCCRole, metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
		// Don't proceed further if no SCC requested by namespace
		return nil
	}

	// We're here, so the namespace has actually requested an SCC
	logger.Infof("Namespace: %s has requested SCC: %s", nsName, nsSCC)

	// Make sure that SCC exists on cluster
	if err := common.VerifySCCExists(ctx, nsSCC, r.securityClientSet); err != nil {
		logger.Error(err)

		// Create an event in the namespace if the SCC does not exist
		eventErr := r.createSCCFailureEventInNamespace(ctx, nsName, nsSCC)
		if eventErr != nil {
			logger.Errorf("Failed to create SCC not found event in namepsace: %s", nsName)
			return eventErr
		}
		return err
	}

	// Make sure SCC requested in the namespace has a lower or equal priority
	// than the SCC mentioned in maxAllowed
	maxAllowedSCC := r.tektonConfig.Spec.Platforms.OpenShift.SCC.MaxAllowed
	if maxAllowedSCC != "" {
		prioritizedSCCList, err := common.GetSCCRestrictiveList(ctx, r.securityClientSet)
		if err != nil {
			return err
		}
		isPriority, err := common.SCCAMoreRestrictiveThanB(prioritizedSCCList, nsSCC, maxAllowedSCC)
		if err != nil {
			return err
		}
		logger.Infof("Is maxAllowed SCC: %s less restrictive than namespace SCC: %s? %t", maxAllowedSCC, nsSCC, isPriority)
		if !isPriority {
			return fmt.Errorf("namespace: %s has requested SCC: %s, but it is less restrictive than the 'maxAllowed' SCC: %s", nsName, nsSCC, maxAllowedSCC)
		}
	}

	// Make sure a Role exists with the SCC attached in the namespace
	if err := r.ensureSCCRoleInNamespace(ctx, nsName, nsSCC); err != nil {
		return err
	}

	return nil
}

// processRBAC encapsulates the logic for processing RBAC in a single namespace.
func (r *rbac) processRBAC(ctx context.Context, ns corev1.Namespace) (*NamespaceServiceAccount, error) {
	logger := logging.FromContext(ctx)
	logger.Infof("Processing RBAC for namespace %s", ns.GetName())

	// Create or update ServiceAccount
	sa, err := r.ensureSA(ctx, &ns)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure ServiceAccount in namespace %s: %v", ns.Name, err)
	}

	if sa == nil {
		return nil, fmt.Errorf("ServiceAccount is nil for namespace %s", ns.Name)
	}

	// Handle SCC in namespace
	if err := r.handleSCCInNamespace(ctx, &ns); err != nil {
		return nil, fmt.Errorf("failed to handle SCC in namespace %s: %v", ns.Name, err)
	}

	// Get and apply role reference
	roleRef := r.getSCCRoleInNamespace(&ns)
	if roleRef != nil {
		if err := r.ensurePipelinesSCCRoleBinding(ctx, sa, roleRef); err != nil {
			return nil, fmt.Errorf("failed to ensure pipelines SCC role binding in namespace %s: %v", ns.Name, err)
		}
	}

	// Ensure role bindings
	if err := r.ensureRoleBindings(ctx, sa); err != nil {
		return nil, fmt.Errorf("failed to ensure role bindings in namespace %s: %v", ns.Name, err)
	}

	return &NamespaceServiceAccount{
		ServiceAccount: sa,
		Namespace:      ns,
	}, nil
}

// patch namespace with reconciled label
func (r *rbac) patchNamespaceLabel(ctx context.Context, ns corev1.Namespace) error {
	logger := logging.FromContext(ctx)

	logger.Infof("add label namespace-reconcile-version to mark namespace '%s' as reconciled", ns.Name)

	// Prepare a patch to add/update just one label without overwriting others
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				namespaceVersionLabel: r.version,
			},
		},
	}

	patchPayload, err := json.Marshal(patch)
	if err != nil {
		logger.Errorf("failed to marshal patch payload: %v", err)
		return fmt.Errorf("failed to marshal label patch for namespace %s: %w", ns.Name, err)
	}

	// Use PATCH to update just the target label
	if _, err := r.kubeClientSet.CoreV1().Namespaces().Patch(ctx, ns.Name, types.StrategicMergePatchType, patchPayload, metav1.PatchOptions{}); err != nil {
		logger.Errorf("failed to patch namespace %s: %v", ns.Name, err)
		return fmt.Errorf("failed to patch namespace %s: %w", ns.Name, err)
	}

	logger.Infof("namespace '%s' successfully reconciled with label %q=%q", ns.Name, namespaceVersionLabel, r.version)
	return nil
}

// createResources handles the reconciliation of RBAC resources and CA bundle configmaps
// across namespaces. It processes each feature independently based on their respective
// configuration flags and only reconciles namespaces that need updates.
func (r *rbac) createResources(ctx context.Context) error {
	logger := logging.FromContext(ctx)

	// Step 1: Check feature flags
	createCABundles := true
	createRBACResource := true

	// Check feature flags
	for _, v := range r.tektonConfig.Spec.Params {
		if v.Name == trustedCABundleParamName && v.Value == "false" {
			createCABundles = false
			logger.Info("CA bundle creation is disabled")
		}
		if v.Name == rbacParamName && v.Value == "false" {
			createRBACResource = false
			logger.Info("RBAC resource creation is disabled")
		}
	}

	// If both features are disabled, nothing to do
	if !createCABundles && !createRBACResource {
		logger.Info("Both CA bundle and RBAC creation are disabled, nothing to do")
		return nil
	}

	// Step 2: Ensure prerequisites (only if RBAC is enabled)
	if createRBACResource {
		if err := r.ensurePreRequisites(ctx); err != nil {
			logger.Errorf("error validating resources: %v", err)
			return err
		}
	}

	// Step 3: Get namespaces to be reconciled for both RBAC and CA bundles
	namespacesToReconcile, err := r.getNamespacesToBeReconciled(ctx)
	if err != nil {
		logger.Error(err)
		return err
	}

	// Early return if no namespaces need reconciliation for either feature
	if len(namespacesToReconcile.RBACNamespaces) == 0 && len(namespacesToReconcile.CANamespaces) == 0 {
		logger.Info("No namespaces need reconciliation for either RBAC or CA bundles")
		return nil
	}

	// Step 4: Handle RBAC if enabled
	if createRBACResource {
		if len(namespacesToReconcile.RBACNamespaces) == 0 {
			logger.Info("No namespaces need RBAC reconciliation")
		} else {
			logger.Debugf("Found %d namespaces to be reconciled for RBAC", len(namespacesToReconcile.RBACNamespaces))

			// Remove and update namespaces from Cluster Interceptors
			if err := r.removeAndUpdateNSFromCI(ctx); err != nil {
				logger.Error(err)
				return err
			}

			var namespacesToUpdate []NamespaceServiceAccount
			// Process each namespace for RBAC
			for _, ns := range namespacesToReconcile.RBACNamespaces {
				logger.Infof("Processing namespace %s for RBAC", ns.Name)
				nsSA, err := r.processRBAC(ctx, ns)
				if err != nil {
					logger.Errorf("failed processing namespace %s: %v", ns.Name, err)
					continue
				}
				namespacesToUpdate = append(namespacesToUpdate, *nsSA)
			}

			// Bulk update ClusterRoleBinding
			if len(namespacesToUpdate) > 0 {
				if err := r.handleClusterRoleBinding(ctx, namespacesToUpdate); err != nil {
					logger.Errorf("failed to ensure clusterrolebinding update: %v", err)
					return err
				}
				logger.Info("Successfully updated cluster role bindings")

				// Patch namespace labels for RBAC
				for _, nsSA := range namespacesToUpdate {
					logger.Infof("Reconciling namespace %s for RBAC", nsSA.Namespace.Name)
					if err := r.patchNamespaceLabel(ctx, nsSA.Namespace); err != nil {
						logger.Errorf("failed reconciling namespace %s: %v", nsSA.Namespace.Name, err)
					}
				}
			}
		}
	}

	// Step 5: Handle CA bundles if enabled
	if createCABundles {
		if len(namespacesToReconcile.CANamespaces) == 0 {
			logger.Info("No namespaces need CA bundle reconciliation")
		} else {
			logger.Debugf("Found %d namespaces to be reconciled for CA bundles", len(namespacesToReconcile.CANamespaces))

			for _, ns := range namespacesToReconcile.CANamespaces {
				logger.Infof("Processing namespace %s for CA bundles", ns.Name)
				if err := r.ensureCABundlesInNamespace(ctx, &ns); err != nil {
					logger.Errorf("failed to ensure CA bundles in namespace %s: %v", ns.Name, err)
					continue
				}
				// Patch namespace with trusted configmaps label
				if err := r.patchNamespaceTrustedConfigLabel(ctx, ns); err != nil {
					logger.Errorf("failed to patch trusted config label for namespace %s: %v", ns.Name, err)
				}
			}
		}
	}

	return nil
}

func (r *rbac) createSCCFailureEventInNamespace(ctx context.Context, namespace string, scc string) error {
	logger := logging.FromContext(ctx)

	failureEvent := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName:    "pipelines-scc-failure-",
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		EventTime:           metav1.NewMicroTime(time.Now()),
		Reason:              "RequestedSCCNotFound",
		Type:                "Warning",
		Action:              "SCCNotUpdated",
		Message:             fmt.Sprintf("SCC '%s' requested in annotation '%s' not found, SCC not updated in the namespace", scc, openshift.NamespaceSCCAnnotation),
		ReportingController: "openshift-pipelines-operator",
		ReportingInstance:   r.ownerRef.Name,
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Namespace",
			Name:       namespace,
			APIVersion: "v1",
			Namespace:  namespace,
		},
	}

	logger.Infof("Creating SCC failure event in namespace: %s", namespace)
	_, err := r.kubeClientSet.CoreV1().Events(namespace).Create(ctx, &failureEvent, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create failure event in namespace %s, %w", namespace, err)
	}

	return nil
}

func (r *rbac) ensureCABundles(ctx context.Context, ns *corev1.Namespace) error {
	logger := logging.FromContext(ctx)
	cfgInterface := r.kubeClientSet.CoreV1().ConfigMaps(ns.Name)

	// Ensure trusted CA bundle
	logger.Infof("finding configmap: %s/%s", ns.Name, trustedCABundleConfigMap)
	caBundleCM, getErr := cfgInterface.Get(ctx, trustedCABundleConfigMap, metav1.GetOptions{})
	if getErr != nil && !errors.IsNotFound(getErr) {
		return getErr
	}

	if getErr != nil && errors.IsNotFound(getErr) {
		logger.Infof("creating configmap %s in %s namespace", trustedCABundleConfigMap, ns.Name)
		var err error
		if caBundleCM, err = createCABundleConfigMaps(ctx, cfgInterface, trustedCABundleConfigMap, ns.Name); err != nil {
			return err
		}
	}

	// If config map already exist then remove owner ref
	if getErr == nil {
		caBundleCM.SetOwnerReferences(nil)
		if _, err := cfgInterface.Update(ctx, caBundleCM, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}

	// Ensure service CA bundle
	logger.Infof("finding configmap: %s/%s", ns.Name, serviceCABundleConfigMap)
	serviceCABundleCM, getErr := cfgInterface.Get(ctx, serviceCABundleConfigMap, metav1.GetOptions{})
	if getErr != nil && !errors.IsNotFound(getErr) {
		return getErr
	}

	if getErr != nil && errors.IsNotFound(getErr) {
		logger.Infof("creating configmap %s in %s namespace", serviceCABundleConfigMap, ns.Name)
		var err error
		if serviceCABundleCM, err = createServiceCABundleConfigMap(ctx, cfgInterface, serviceCABundleConfigMap, ns.Name); err != nil {
			return err
		}
	}

	// If config map already exist then remove owner ref
	if getErr == nil {
		serviceCABundleCM.SetOwnerReferences(nil)
		if _, err := cfgInterface.Update(ctx, serviceCABundleCM, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}

	return nil
}

func createCABundleConfigMaps(ctx context.Context, cfgInterface v1.ConfigMapInterface,
	name, ns string) (*corev1.ConfigMap, error) {
	c := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "tekton-pipelines",
				// user-provided and system CA certificates
				"config.openshift.io/inject-trusted-cabundle": "true",
			},
			// No OwnerReferences
		},
	}

	cm, err := cfgInterface.Create(ctx, c, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}
	return cm, nil
}

func createServiceCABundleConfigMap(ctx context.Context, cfgInterface v1.ConfigMapInterface,
	name, ns string) (*corev1.ConfigMap, error) {
	c := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "tekton-pipelines",
			},
			Annotations: map[string]string{
				// service serving certificates (required to talk to the internal registry)
				"service.beta.openshift.io/inject-cabundle": "true",
			},
			// No OwnerReferences
		},
	}

	cm, err := cfgInterface.Create(ctx, c, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}
	return cm, nil
}

func (r *rbac) ensureSA(ctx context.Context, ns *corev1.Namespace) (*corev1.ServiceAccount, error) {
	logger := logging.FromContext(ctx)
	logger.Infof("finding sa: %s/%s", ns.Name, "pipeline")
	saInterface := r.kubeClientSet.CoreV1().ServiceAccounts(ns.Name)

	sa, err := saInterface.Get(ctx, pipelineSA, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	if err != nil && errors.IsNotFound(err) {
		logger.Info("creating sa ", pipelineSA, " ns", ns.Name)
		return createSA(ctx, saInterface, ns.Name, *r.tektonConfig)
	}

	// set tektonConfig ownerRef
	tcOwnerRef := tektonConfigOwnerRef(*r.tektonConfig)
	sa.SetOwnerReferences([]metav1.OwnerReference{tcOwnerRef})

	return saInterface.Update(ctx, sa, metav1.UpdateOptions{})
}

func createSA(ctx context.Context, saInterface v1.ServiceAccountInterface, ns string, tc v1alpha1.TektonConfig) (*corev1.ServiceAccount, error) {
	tcOwnerRef := tektonConfigOwnerRef(tc)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pipelineSA,
			Namespace:       ns,
			OwnerReferences: []metav1.OwnerReference{tcOwnerRef},
		},
	}

	sa, err := saInterface.Create(ctx, sa, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	// Initialize labels map if it doesn't exist
	if tc.Labels == nil {
		tc.Labels = make(map[string]string)
	}
	tc.Labels[serviceAccountCreationLabel] = "true"
	return sa, nil
}

// ensureSCCRoleInNamespace ensures that the SCC role exists in the namespace
func (r *rbac) ensureSCCRoleInNamespace(ctx context.Context, namespace string, scc string) error {
	logger := logging.FromContext(ctx)

	logger.Infof("finding role: %s in namespace %s", pipelinesSCCRole, namespace)

	sccRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pipelinesSCCRole,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{
					"security.openshift.io",
				},
				ResourceNames: []string{
					scc,
				},
				Resources: []string{
					"securitycontextconstraints",
				},
				Verbs: []string{
					"use",
				},
			},
		},
	}

	rbacClient := r.kubeClientSet.RbacV1()
	if _, err := rbacClient.Roles(namespace).Get(ctx, pipelinesSCCRole, metav1.GetOptions{}); err != nil {
		// If the role does not exist, then create it and exit
		if errors.IsNotFound(err) {
			_, err = rbacClient.Roles(namespace).Create(ctx, sccRole, metav1.CreateOptions{})
		}
		return err
	}
	// Update the role if it already exists
	_, err := rbacClient.Roles(namespace).Update(ctx, sccRole, metav1.UpdateOptions{})
	return err
}

// ensurePipelinesSCClusterRole ensures that `pipelines-scc` ClusterRole exists
// in the cluster. The SCC used in the ClusterRole is read from SCC config
// in TektonConfig (`pipelines-scc` by default)
func (r *rbac) ensurePipelinesSCClusterRole(ctx context.Context) error {
	logger := logging.FromContext(ctx)

	logger.Info("finding cluster role:", pipelinesSCCClusterRole)

	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pipelinesSCCClusterRole,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{
					"security.openshift.io",
				},
				ResourceNames: []string{
					r.tektonConfig.Spec.Platforms.OpenShift.SCC.Default,
				},
				Resources: []string{
					"securitycontextconstraints",
				},
				Verbs: []string{
					"use",
				},
			},
		},
	}

	rbacClient := r.kubeClientSet.RbacV1()
	if _, err := rbacClient.ClusterRoles().Get(ctx, pipelinesSCCClusterRole, metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			_, err = rbacClient.ClusterRoles().Create(ctx, clusterRole, metav1.CreateOptions{})
		}
		return err
	}
	_, err := rbacClient.ClusterRoles().Update(ctx, clusterRole, metav1.UpdateOptions{})
	return err
}

func (r *rbac) ensurePipelinesSCCRoleBinding(ctx context.Context, sa *corev1.ServiceAccount, roleRef *rbacv1.RoleRef) error {
	logger := logging.FromContext(ctx)
	rbacClient := r.kubeClientSet.RbacV1()

	roleKind := roleRef.Kind
	roleName := roleRef.Name
	if roleRef.Kind == "Role" {
		logger.Infof("finding %s: %s", roleKind, roleName)
		if _, err := rbacClient.Roles(sa.Namespace).Get(ctx, roleName, metav1.GetOptions{}); err != nil {
			logger.Error(err, "finding %s failed: %s", roleKind, roleName)
			return err
		}
	} else if roleKind == "ClusterRole" {
		logger.Infof("finding %s: %s", roleKind, roleName)
		if _, err := rbacClient.ClusterRoles().Get(ctx, roleName, metav1.GetOptions{}); err != nil {
			logger.Error(err, "finding %s failed: %s", roleKind, roleName)
			return err
		}
	} else {
		return fmt.Errorf("incorrect value set for roleKind - %s, needs to be Role or ClusterRole", roleKind)
	}

	logger.Info("finding role-binding", pipelinesSCCRoleBinding)
	pipelineRB, rbErr := rbacClient.RoleBindings(sa.Namespace).Get(ctx, pipelinesSCCRoleBinding, metav1.GetOptions{})
	if rbErr != nil && !errors.IsNotFound(rbErr) {
		logger.Error(rbErr, "rbac get error", pipelinesSCCRoleBinding)
		return rbErr
	}

	if rbErr != nil && errors.IsNotFound(rbErr) {
		return r.createSCCRoleBinding(ctx, sa, roleRef)
	}

	// We cannot update RoleRef in a RoleBinding, we need to delete and
	// recreate the binding in that case
	if pipelineRB.RoleRef.Kind != roleKind || pipelineRB.RoleRef.Name != roleName {
		logger.Infof("Need to update RoleRef in RoleBinding %s in namespace: %s, deleting and recreating...", pipelinesSCCRoleBinding, sa.Namespace)
		err := rbacClient.RoleBindings(sa.Namespace).Delete(ctx, pipelinesSCCRoleBinding, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		return r.createSCCRoleBinding(ctx, sa, roleRef)
	}

	logger.Info("found rbac", "subjects", pipelineRB.Subjects)
	return r.updateRoleBinding(ctx, pipelineRB, sa, roleRef)
}

func (r *rbac) createSCCRoleBinding(ctx context.Context, sa *corev1.ServiceAccount, roleRef *rbacv1.RoleRef) error {
	logger := logging.FromContext(ctx)
	rbacClient := r.kubeClientSet.RbacV1()

	logger.Info("create new rolebinding:", pipelinesSCCRoleBinding)
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pipelinesSCCRoleBinding,
			Namespace:       sa.Namespace,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		RoleRef:  *roleRef,
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: sa.Namespace}},
	}

	_, err := rbacClient.RoleBindings(sa.Namespace).Create(ctx, rb, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "creation of rolebinding failed:", pipelinesSCCRoleBinding)
	}
	return err
}

func (r *rbac) updateRoleBinding(ctx context.Context, rb *rbacv1.RoleBinding, sa *corev1.ServiceAccount, roleRef *rbacv1.RoleRef) error {
	logger := logging.FromContext(ctx)

	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: sa.Namespace}

	hasSubject := hasSubject(rb.Subjects, subject)
	if !hasSubject {
		rb.Subjects = append(rb.Subjects, subject)
	}

	rb.RoleRef = *roleRef

	rbacClient := r.kubeClientSet.RbacV1()
	hasOwnerRef := hasOwnerRefernce(rb.GetOwnerReferences(), r.ownerRef)

	ownerRef := r.updateOwnerRefs(rb.GetOwnerReferences())
	rb.SetOwnerReferences(ownerRef)

	// If owners are different then we need to set from r.ownerRef and update the roleBinding.
	if !hasOwnerRef {
		if _, err := rbacClient.RoleBindings(sa.Namespace).Update(ctx, rb, metav1.UpdateOptions{}); err != nil {
			logger.Error(err, "failed to update edit rb")
			return err
		}
	}

	if hasSubject && (len(ownerRef) != 0) {
		logger.Info("rolebinding is up to date ", "action ", "none")
		return nil
	}

	logger.Infof("update existing rolebinding %s/%s", rb.Namespace, rb.Name)

	_, err := rbacClient.RoleBindings(sa.Namespace).Update(ctx, rb, metav1.UpdateOptions{})
	if err != nil {
		logger.Errorf("%v: failed to update rolebinding %s/%s", err, rb.Namespace, rb.Name)
		return err
	}
	logger.Infof("successfully updated rolebinding %s/%s", rb.Namespace, rb.Name)
	return nil
}

func hasSubject(subjects []rbacv1.Subject, x rbacv1.Subject) bool {
	for _, v := range subjects {
		if v.Name == x.Name && v.Kind == x.Kind && v.Namespace == x.Namespace {
			return true
		}
	}
	return false
}

// CompareLists compares two slices of rbacv1.Subject, ignoring order
func CompareSubjects(list1, list2 []rbacv1.Subject) bool {
	// Check if lengths are different
	if len(list1) != len(list2) {
		return false
	}
	// Create sets (maps) for both lists
	set1 := make(map[string]struct{})
	set2 := make(map[string]struct{})

	// Populate set1 with subjects from list1
	for _, subject := range list1 {
		key := fmt.Sprintf("%s/%s/%s", subject.Kind, subject.Name, subject.Namespace)
		set1[key] = struct{}{}
	}
	// Populate set2 with subjects from list2
	for _, subject := range list2 {
		key := fmt.Sprintf("%s/%s/%s", subject.Kind, subject.Name, subject.Namespace)
		set2[key] = struct{}{}
	}

	// Compare the sets
	if len(set1) != len(set2) {
		return false
	}

	// Check if all elements in set1 are in set2
	for key := range set1 {
		if _, exists := set2[key]; !exists {
			return false
		}
	}
	return true
}

func mergeSubjects(subjects []rbacv1.Subject, x []rbacv1.Subject) []rbacv1.Subject {
	// Map to track subjects in the existing list
	existingSubjects := make(map[string]struct{})

	// Iterate over `subjects` and track each unique combination of Kind, Name, and Namespace
	for _, subject := range subjects {
		key := fmt.Sprintf("%s/%s/%s", subject.Kind, subject.Name, subject.Namespace)
		existingSubjects[key] = struct{}{}
	}

	// Final list to store the merged subjects
	var finalSubjects []rbacv1.Subject

	// Add all subjects from the original list (list1)
	finalSubjects = append(finalSubjects, subjects...)

	// Append subjects from `x` (list2) that are not in `existingSubjects`
	for _, subject := range x {
		key := fmt.Sprintf("%s/%s/%s", subject.Kind, subject.Name, subject.Namespace)
		if _, found := existingSubjects[key]; !found {
			finalSubjects = append(finalSubjects, subject)
		}
	}

	return finalSubjects
}

func hasOwnerRefernce(old []metav1.OwnerReference, new metav1.OwnerReference) bool {
	for _, v := range old {
		if v.APIVersion == new.APIVersion && v.Kind == new.Kind && v.Name == new.Name {
			return true
		}
	}
	return false
}

func (r *rbac) isLegacyRBACEnabled() bool {
	for _, v := range r.tektonConfig.Spec.Params {
		if v.Name == legacyPipelineRbacParamName {
			return v.Value != "false"
		}
	}
	return true
}

func (r *rbac) ensureRoleBindings(ctx context.Context, sa *corev1.ServiceAccount) error {
	logger := logging.FromContext(ctx)
	rbacClient := r.kubeClientSet.RbacV1()

	legacyEnabled := r.isLegacyRBACEnabled()

	editRB, err := rbacClient.RoleBindings(sa.Namespace).Get(ctx, PipelineRoleBinding, metav1.GetOptions{})

	if !legacyEnabled && err == nil {
		logger.Infof("Legacy Pipeline RBAC is disabled, removing existing role binding %s/%s",
			editRB.Namespace, editRB.Name)
		return rbacClient.RoleBindings(sa.Namespace).Delete(ctx, PipelineRoleBinding, metav1.DeleteOptions{})
	}

	if !legacyEnabled {
		logger.Infof("Legacy Pipeline RBAC is disabled, skipping role binding creation")
		return nil
	}

	logger.Infof("Legacy Pipeline RBAC is enabled")

	if err == nil {
		logger.Infof("Found rolebinding %s/%s, updating if needed", editRB.Namespace, editRB.Name)
		return r.updateRoleBinding(ctx, editRB, sa, &rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "edit",
		})
	}

	if errors.IsNotFound(err) {
		logger.Infof("Role binding not found, creating new one")
		return r.createRoleBinding(ctx, sa)
	}

	return err
}

func (r *rbac) createRoleBinding(ctx context.Context, sa *corev1.ServiceAccount) error {
	logger := logging.FromContext(ctx)

	logger.Infof("create new rolebinding %s/%s", sa.Namespace, sa.Name)
	rbacClient := r.kubeClientSet.RbacV1()

	logger.Info("finding clusterrole edit")
	if _, err := rbacClient.ClusterRoles().Get(ctx, "edit", metav1.GetOptions{}); err != nil {
		logger.Error(err, "getting clusterRole 'edit' failed")
		return err
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            PipelineRoleBinding,
			Namespace:       sa.Namespace,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: sa.Namespace}},
	}

	if _, err := rbacClient.RoleBindings(sa.Namespace).Create(ctx, rb, metav1.CreateOptions{}); err != nil {
		logger.Errorf("%v: failed creation of rolebinding %s/%s", err, rb.Namespace, rb.Name)
		return err
	}
	return nil
}

func (r *rbac) removeAndUpdateNSFromCI(ctx context.Context) error {
	logger := logging.FromContext(ctx)

	rbacClient := r.kubeClientSet.RbacV1()
	rb, err := r.rbacInformer.Lister().Get(clusterInterceptors)
	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "failed to get"+clusterInterceptors)
		return err
	}
	if rb == nil {
		return nil
	}

	req, err := labels.NewRequirement(namespaceVersionLabel, selection.Equals, []string{r.version})
	if err != nil {
		logger.Error(err, "failed to create requirement: ")
		return err
	}

	namespaces, err := r.nsInformer.Lister().List(labels.NewSelector().Add(*req))
	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "failed to list namespace: ")
		return err
	}

	nsMap := map[string]string{}
	for i := range namespaces {
		nsMap[namespaces[i].Name] = namespaces[i].Name
	}

	var update bool
	for i := 0; i <= len(rb.Subjects)-1; i++ {
		if len(nsMap) != len(rb.Subjects) {
			if _, ok := nsMap[rb.Subjects[i].Namespace]; !ok {
				rb.Subjects = removeIndex(rb.Subjects, i)
				update = true
			}
		}
	}
	if update {
		if _, err := rbacClient.ClusterRoleBindings().Update(ctx, rb, metav1.UpdateOptions{}); err != nil {
			logger.Error(err, "failed to update "+clusterInterceptors+" crb")
			return err
		}
		logger.Infof("successfully removed namespace and updated %s ", clusterInterceptors)
	}
	return nil
}

func removeIndex(s []rbacv1.Subject, index int) []rbacv1.Subject {
	return append(s[:index], s[index+1:]...)
}

func (r *rbac) handleClusterRoleBinding(ctx context.Context, namespacesToUpdate []NamespaceServiceAccount) error {
	logger := logging.FromContext(ctx)

	rbacClient := r.kubeClientSet.RbacV1()
	logger.Info("finding cluster-role ", clusterInterceptors)
	if _, err := rbacClient.ClusterRoles().Get(ctx, clusterInterceptors, metav1.GetOptions{}); errors.IsNotFound(err) {
		if e := r.createClusterRole(ctx); e != nil {
			return e
		}
	}

	// Prepare a list of Subjects from the namespacesToUpdate
	var subjects []rbacv1.Subject

	for _, nsSA := range namespacesToUpdate {
		sa := nsSA.ServiceAccount
		ns := nsSA.Namespace

		logger.Infof("Processing Subject for ServiceAccount %s in Namespace %s", sa.Name, ns.Name)

		// Create the Subject for the ClusterRoleBinding
		subject := rbacv1.Subject{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      sa.Name,
			Namespace: sa.Namespace,
		}

		// Append the subject to the list
		subjects = append(subjects, subject)
	}

	logger.Info("finding cluster-role-binding ", clusterInterceptors)

	viewCRB, err := rbacClient.ClusterRoleBindings().Get(ctx, clusterInterceptors, metav1.GetOptions{})

	if err == nil {
		logger.Infof("found clusterrolebinding %s", viewCRB.Name)
		return r.bulkUpdateClusterRoleBinding(ctx, viewCRB, subjects)
	}

	if errors.IsNotFound(err) {
		logger.Infof("could not find clusterrolebinding %s proceeding to create", viewCRB.Name)
		return r.bulkCreateClusterRoleBinding(ctx, subjects)
	}

	return err
}

// bulk update Cluster rolebinding with all reconciled namespaces and service accounts
func (r *rbac) bulkUpdateClusterRoleBinding(ctx context.Context, rb *rbacv1.ClusterRoleBinding, subjectlist []rbacv1.Subject) error {
	logger := logging.FromContext(ctx)

	hasSubject := CompareSubjects(rb.Subjects, subjectlist)
	if !hasSubject {
		rb.Subjects = mergeSubjects(rb.Subjects, subjectlist)
	}

	rbacClient := r.kubeClientSet.RbacV1()
	hasOwnerRef := hasOwnerRefernce(rb.GetOwnerReferences(), r.ownerRef)

	ownerRef := r.updateOwnerRefs(rb.GetOwnerReferences())
	rb.SetOwnerReferences(ownerRef)

	// If owners are different then we need to set from r.ownerRef and update the clusterRolebinding.
	if !hasOwnerRef {
		if _, err := rbacClient.ClusterRoleBindings().Update(ctx, rb, metav1.UpdateOptions{}); err != nil {
			logger.Error(err, "failed to update "+clusterInterceptors+" crb")
			return err
		}
	}

	if hasSubject && (len(ownerRef) != 0) {
		logger.Info("clusterrolebinding is up to date", "action", "none")
		return nil
	}

	logger.Info("update existing clusterrolebinding ", clusterInterceptors)

	if _, err := rbacClient.ClusterRoleBindings().Update(ctx, rb, metav1.UpdateOptions{}); err != nil {
		logger.Error(err, "failed to update "+clusterInterceptors+" crb")
		return err
	}
	logger.Info("successfully updated ", clusterInterceptors)
	return nil
}

// create Cluster rolebinding with all reconciled namespaces and service accounts
func (r *rbac) bulkCreateClusterRoleBinding(ctx context.Context, subjectlist []rbacv1.Subject) error {
	logger := logging.FromContext(ctx)

	logger.Info("create new clusterrolebinding ", clusterInterceptors)
	rbacClient := r.kubeClientSet.RbacV1()

	logger.Info("finding clusterrole ", clusterInterceptors)
	if _, err := rbacClient.ClusterRoles().Get(ctx, clusterInterceptors, metav1.GetOptions{}); err != nil {
		logger.Error(err, " getting clusterRole "+clusterInterceptors+" failed")
		return err
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            clusterInterceptors,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterInterceptors},
		Subjects: subjectlist,
	}

	if _, err := rbacClient.ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
		logger.Error(err, " creation of "+clusterInterceptors+" failed")
		return err
	}
	return nil
}

func (r *rbac) createClusterRole(ctx context.Context) error {
	logger := logging.FromContext(ctx)

	logger.Info("create new clusterrole ", clusterInterceptors)
	rbacClient := r.kubeClientSet.RbacV1()

	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:            clusterInterceptors,
			OwnerReferences: []metav1.OwnerReference{r.ownerRef},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"triggers.tekton.dev"},
			Resources: []string{"clusterinterceptors"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}

	if _, err := rbacClient.ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		logger.Error(err, "creation of "+clusterInterceptors+" clusterrole failed")
		return err
	}
	return nil
}

func (r *rbac) updateOwnerRefs(ownerRef []metav1.OwnerReference) []metav1.OwnerReference {
	if len(ownerRef) == 0 {
		return []metav1.OwnerReference{r.ownerRef}
	}

	for i, ref := range ownerRef {
		if ref.APIVersion != r.ownerRef.APIVersion || ref.Kind != r.ownerRef.Kind || ref.Name != r.ownerRef.Name {
			// if owner reference are different remove the existing oand override with r.ownerRef
			return r.removeAndUpdate(ownerRef, i)
		}
	}

	return ownerRef
}

func (r *rbac) removeAndUpdate(slice []metav1.OwnerReference, s int) []metav1.OwnerReference {
	ownerRef := append(slice[:s], slice[s+1:]...)
	ownerRef = append(ownerRef, r.ownerRef)
	return ownerRef
}

// TODO: Remove this after v0.55.0 release, by following a depreciation notice
// --------------------
// cleanUpRBACNameChange will check remove ownerReference: RBAC installerset from
// 'edit' rolebindings from all relevant namespaces
// it will also remove 'pipeline' sa from subject list as
// the new 'openshift-pipelines-edit' rolebinding
func (r *rbac) cleanUpRBACNameChange(ctx context.Context) error {
	rbacClient := r.kubeClientSet.RbacV1()

	// fetch the list of all namespaces
	namespaces, err := r.kubeClientSet.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, ns := range namespaces.Items {
		nsName := ns.GetName()

		// filter namespaces:
		// ignore ns with name passing regex `^(openshift|kube)-`
		if ignore := nsRegex.MatchString(nsName); ignore {
			continue
		}

		// check if "edit" rolebinding exists in "ns" namespace
		editRB, err := rbacClient.RoleBindings(ns.GetName()).
			Get(ctx, pipelineRoleBindingOld, metav1.GetOptions{})
		if err != nil {
			// if "edit" rolebinding does not exists in "ns" namesapce, then do nothing
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}

		// check if 'pipeline' serviceaccount is listed as a subject in 'edit' rolebinding
		depSub := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: pipelineSA, Namespace: nsName}
		subIdx := math.MinInt16
		for i, s := range editRB.Subjects {
			if s.Name == depSub.Name && s.Kind == depSub.Kind && s.Namespace == depSub.Namespace {
				subIdx = i
				break
			}
		}

		// if 'pipeline' serviceaccount is listed as a subject in 'edit' rolebinding
		// remove 'pipeline' serviceaccount from subject list
		if subIdx >= 0 {
			editRB.Subjects = append(editRB.Subjects[:subIdx], editRB.Subjects[subIdx+1:]...)
		}

		// if 'pipeline' serviceaccount was the only item in the subject list of 'edit' rolebinding,
		// then we can delete 'edit' rolebinding as nobody else is using it
		if len(editRB.Subjects) == 0 {
			if err := rbacClient.RoleBindings(nsName).Delete(ctx, editRB.GetName(), metav1.DeleteOptions{}); err != nil {
				return err
			}
			continue
		}

		// remove TektonInstallerSet ownerReferece from "edit" rolebinding
		ownerRefs := editRB.GetOwnerReferences()
		ownerRefIdx := math.MinInt16
		for i, ownerRef := range ownerRefs {
			if ownerRef.Kind == "TektonInstallerSet" {
				ownerRefIdx = i
				break
			}
		}
		if ownerRefIdx >= 0 {
			ownerRefs := append(ownerRefs[:ownerRefIdx], ownerRefs[ownerRefIdx+1:]...)
			editRB.SetOwnerReferences(ownerRefs)

		}

		// if ownerReference or subject was updated, then update editRB resource on cluster
		if ownerRefIdx < 0 && subIdx < 0 {
			continue
		}
		if _, err := rbacClient.RoleBindings(nsName).Update(ctx, editRB, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// TODO: Remove this after v0.55.0 release, by following a depreciation notice
// --------------------
func (r *rbac) removeObsoleteRBACInstallerSet(ctx context.Context) error {
	isClient := r.operatorClientSet.OperatorV1alpha1().TektonInstallerSets()
	err := isClient.Delete(ctx, rbacInstallerSetNameOld, metav1.DeleteOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *rbac) ensureCABundlesInNamespace(ctx context.Context, ns *corev1.Namespace) error {
	logger := logging.FromContext(ctx)
	logger.Infow("Ensuring CA bundle configmaps in namespace", "namespace", ns.GetName())
	return r.ensureCABundles(ctx, ns)
}

// Add new method for patching namespace with trusted configmaps label
func (r *rbac) patchNamespaceTrustedConfigLabel(ctx context.Context, ns corev1.Namespace) error {
	logger := logging.FromContext(ctx)

	logger.Infof("add label namespace-trusted-configmaps-version to mark namespace '%s' as reconciled", ns.Name)

	// Prepare a patch to add/update just one label without overwriting others
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				namespaceTrustedConfigLabel: r.version,
			},
		},
	}

	patchPayload, err := json.Marshal(patch)
	if err != nil {
		logger.Errorf("failed to marshal patch payload: %v", err)
		return fmt.Errorf("failed to marshal label patch for namespace %s: %w", ns.Name, err)
	}

	// Use PATCH to update just the target label
	if _, err := r.kubeClientSet.CoreV1().Namespaces().Patch(ctx, ns.Name, types.StrategicMergePatchType, patchPayload, metav1.PatchOptions{}); err != nil {
		logger.Errorf("failed to patch namespace %s: %v", ns.Name, err)
		return fmt.Errorf("failed to patch namespace %s: %w", ns.Name, err)
	}

	logger.Infof("namespace '%s' successfully reconciled with label %q=%q", ns.Name, namespaceTrustedConfigLabel, r.version)
	return nil
}
