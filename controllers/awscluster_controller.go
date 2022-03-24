/*
Copyright 2019 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-aws/feature"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/ec2"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/elb"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/instancestate"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/network"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/s3"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/securitygroup"
	infrautilconditions "sigs.k8s.io/cluster-api-provider-aws/util/conditions"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
)

var (
	awsSecurityGroupRoles = []infrav1.SecurityGroupRole{
		infrav1.SecurityGroupBastion,
		infrav1.SecurityGroupAPIServerLB,
		infrav1.SecurityGroupLB,
		infrav1.SecurityGroupControlPlane,
		infrav1.SecurityGroupNode,
	}
)

type awsClusterReconcilerOption func(*awsClusterReconciler)

// awsClusterReconciler reconciles a AWSCluster object.
type awsClusterReconciler struct {
	client.Client
	Recorder              record.EventRecorder
	ec2ServiceFactory     awsClusterEC2ServiceFactory
	networkServiceFactory awsClusterNetworkServiceFactory
	elbServiceFactory     awsClusterELBServiceFactory
	securityGroupFactory  awsClusterSecurityGroupServiceFactory
	objectStoreFactory    awsClusterObjectStoreServiceFactory
	Endpoints             []scope.ServiceEndpoint
	WatchFilterValue      string
}

type NewClusterReconcilerInput struct {
	Manager          ctrl.Manager
	WatchFilterValue string
	Endpoints        []scope.ServiceEndpoint
}

type awsClusterEC2ServiceFactory func(scope.EC2Scope) services.EC2Interface

func withAWSClusterEC2ServiceFactory(fn awsClusterEC2ServiceFactory) awsClusterReconcilerOption {
	return func(r *awsClusterReconciler) {
		r.ec2ServiceFactory = fn
	}
}

type awsClusterELBServiceFactory func(scope.ELBScope) services.ELBInterface

func withAWSClusterELBServiceFactory(fn awsClusterELBServiceFactory) awsClusterReconcilerOption {
	return func(r *awsClusterReconciler) {
		r.elbServiceFactory = fn
	}
}

type awsClusterNetworkServiceFactory func(scope.ClusterScope) services.NetworkInterface

func withAWSClusterNetworkServiceFactory(fn awsClusterNetworkServiceFactory) awsClusterReconcilerOption {
	return func(r *awsClusterReconciler) {
		r.networkServiceFactory = fn
	}
}

type awsClusterSecurityGroupServiceFactory func(scope.ClusterScope) services.SecurityGroupInterface

func withAWSClusterSecurityGroupServiceFactory(fn awsClusterSecurityGroupServiceFactory) awsClusterReconcilerOption {
	return func(r *awsClusterReconciler) {
		r.securityGroupFactory = fn
	}
}

type awsClusterObjectStoreServiceFactory func(scope.ClusterScope) services.ObjectStoreInterface

func NewClusterReconciler(input NewClusterReconcilerInput, opt ...awsClusterReconcilerOption) *awsClusterReconciler {
	clusterReconciler := &awsClusterReconciler{
		Client:           input.Manager.GetClient(),
		Recorder:         input.Manager.GetEventRecorderFor("awscluster-controller"),
		Endpoints:        input.Endpoints,
		WatchFilterValue: input.WatchFilterValue,
		ec2ServiceFactory: func(ec2Scope scope.EC2Scope) services.EC2Interface {
			return ec2.NewService(ec2Scope)
		},
		networkServiceFactory: func(clusterScope scope.ClusterScope) services.NetworkInterface {
			return network.NewService(&clusterScope)
		},
		elbServiceFactory: func(elbScope scope.ELBScope) services.ELBInterface {
			return elb.NewService(elbScope)
		},
		securityGroupFactory: func(clusterScope scope.ClusterScope) services.SecurityGroupInterface {
			return securitygroup.NewService(&clusterScope, awsSecurityGroupRoles)
		},
		objectStoreFactory: func(clusterScope scope.ClusterScope) services.ObjectStoreInterface {
			return s3.NewService(&clusterScope)
		},
	}

	for _, opt := range opt {
		opt(clusterReconciler)
	}

	return clusterReconciler
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusterroleidentities;awsclusterstaticidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclustercontrolleridentities,verbs=get;list;watch;create;

func (r *awsClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the AWSCluster instance
	awsCluster := &infrav1.AWSCluster{}
	err := r.Get(ctx, req.NamespacedName, awsCluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, awsCluster.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}

	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return reconcile.Result{}, nil
	}

	if annotations.IsPaused(cluster, awsCluster) {
		log.Info("AWSCluster or linked Cluster is marked as paused. Won't reconcile")
		return reconcile.Result{}, nil
	}

	log = log.WithValues("cluster", cluster.Name)
	helper, err := patch.NewHelper(awsCluster, r.Client)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to init patch helper")
	}

	defer func() {
		e := helper.Patch(
			context.TODO(),
			awsCluster,
			patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
				infrav1.PrincipalCredentialRetrievedCondition,
				infrav1.PrincipalUsageAllowedCondition,
			}})
		if e != nil {
			fmt.Println(e.Error())
		}
	}()

	// Create the scope.
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:         r.Client,
		Logger:         &log,
		Cluster:        cluster,
		AWSCluster:     awsCluster,
		ControllerName: "awscluster",
		Endpoints:      r.Endpoints,
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any AWSCluster changes.
	defer func() {
		if err := clusterScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Handle deleted clusters
	if !awsCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(clusterScope)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(clusterScope)
}

func (r *awsClusterReconciler) reconcileDelete(clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	clusterScope.Info("Reconciling AWSCluster delete")

	ec2Service := r.ec2ServiceFactory(clusterScope)
	elbService := r.elbServiceFactory(clusterScope)
	networkService := r.networkServiceFactory(*clusterScope)
	sgService := r.securityGroupFactory(*clusterScope)
	s3Service := r.objectStoreFactory(*clusterScope)

	if feature.Gates.Enabled(feature.EventBridgeInstanceState) {
		instancestateSvc := instancestate.NewService(clusterScope)
		if err := instancestateSvc.DeleteEC2Events(); err != nil {
			// Not deleting the events isn't critical to cluster deletion
			clusterScope.Error(err, "non-fatal: failed to delete EventBridge notifications")
		}
	}

	if err := elbService.DeleteLoadbalancers(); err != nil {
		clusterScope.Error(err, "error deleting load balancer")
		return reconcile.Result{}, err
	}

	if err := ec2Service.DeleteBastion(); err != nil {
		clusterScope.Error(err, "error deleting bastion")
		return reconcile.Result{}, err
	}

	if err := sgService.DeleteSecurityGroups(); err != nil {
		clusterScope.Error(err, "error deleting security groups")
		return reconcile.Result{}, err
	}

	if err := networkService.DeleteNetwork(); err != nil {
		clusterScope.Error(err, "error deleting network")
		return reconcile.Result{}, err
	}

	if err := s3Service.DeleteBucket(); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "error deleting S3 Bucket")
	}

	// Cluster is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(clusterScope.AWSCluster, infrav1.ClusterFinalizer)

	return reconcile.Result{}, nil
}

func (r *awsClusterReconciler) reconcileNormal(clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	clusterScope.Info("Reconciling AWSCluster")

	awsCluster := clusterScope.AWSCluster

	// If the AWSCluster doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(awsCluster, infrav1.ClusterFinalizer)
	// Register the finalizer immediately to avoid orphaning AWS resources on delete
	if err := clusterScope.PatchObject(); err != nil {
		return reconcile.Result{}, err
	}

	ec2Service := r.ec2ServiceFactory(clusterScope)
	elbService := r.elbServiceFactory(clusterScope)
	networkService := r.networkServiceFactory(*clusterScope)
	sgService := r.securityGroupFactory(*clusterScope)
	s3Service := r.objectStoreFactory(*clusterScope)

	if err := networkService.ReconcileNetwork(); err != nil {
		clusterScope.Error(err, "failed to reconcile network")
		return reconcile.Result{}, err
	}

	// CNI related security groups gets deleted from the AWSClusters created prior to networkSpec.cni defaulting (5.5) after upgrading controllers.
	// https://github.com/kubernetes-sigs/cluster-api-provider-aws/issues/2084
	// TODO: Remove this after v1aplha4
	clusterScope.AWSCluster.Default()

	if err := sgService.ReconcileSecurityGroups(); err != nil {
		clusterScope.Error(err, "failed to reconcile security groups")
		conditions.MarkFalse(awsCluster, infrav1.ClusterSecurityGroupsReadyCondition, infrav1.ClusterSecurityGroupReconciliationFailedReason, infrautilconditions.ErrorConditionAfterInit(clusterScope.ClusterObj()), err.Error())
		return reconcile.Result{}, err
	}

	if err := ec2Service.ReconcileBastion(); err != nil {
		conditions.MarkFalse(awsCluster, infrav1.BastionHostReadyCondition, infrav1.BastionHostFailedReason, infrautilconditions.ErrorConditionAfterInit(clusterScope.ClusterObj()), err.Error())
		clusterScope.Error(err, "failed to reconcile bastion host")
		return reconcile.Result{}, err
	}

	if feature.Gates.Enabled(feature.EventBridgeInstanceState) {
		instancestateSvc := instancestate.NewService(clusterScope)
		if err := instancestateSvc.ReconcileEC2Events(); err != nil {
			// non fatal error, so we continue
			clusterScope.Error(err, "non-fatal: failed to set up EventBridge")
		}
	}

	if err := elbService.ReconcileLoadbalancers(); err != nil {
		clusterScope.Error(err, "failed to reconcile load balancer")
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.LoadBalancerFailedReason, infrautilconditions.ErrorConditionAfterInit(clusterScope.ClusterObj()), err.Error())
		return reconcile.Result{}, err
	}

	if err := s3Service.ReconcileBucket(); err != nil {
		conditions.MarkFalse(awsCluster, infrav1.S3BucketReadyCondition, infrav1.S3BucketFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, errors.Wrapf(err, "failed to reconcile S3 Bucket for AWSCluster %s/%s", awsCluster.Namespace, awsCluster.Name)
	}

	if awsCluster.Status.Network.APIServerELB.DNSName == "" {
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.WaitForDNSNameReason, clusterv1.ConditionSeverityInfo, "")
		clusterScope.Info("Waiting on API server ELB DNS name")
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if _, err := net.LookupIP(awsCluster.Status.Network.APIServerELB.DNSName); err != nil {
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.WaitForDNSNameResolveReason, clusterv1.ConditionSeverityInfo, "")
		clusterScope.Info("Waiting on API server ELB DNS name to resolve")
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil // nolint:nilerr
	}
	conditions.MarkTrue(awsCluster, infrav1.LoadBalancerReadyCondition)

	awsCluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
		Host: awsCluster.Status.Network.APIServerELB.DNSName,
		Port: clusterScope.APIServerPort(),
	}

	for _, subnet := range clusterScope.Subnets().FilterPrivate() {
		found := false
		for _, az := range awsCluster.Status.Network.APIServerELB.AvailabilityZones {
			if az == subnet.AvailabilityZone {
				found = true
				break
			}
		}

		clusterScope.SetFailureDomain(subnet.AvailabilityZone, clusterv1.FailureDomainSpec{
			ControlPlane: found,
		})
	}

	awsCluster.Status.Ready = true
	return reconcile.Result{}, nil
}

func (r *awsClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := ctrl.LoggerFrom(ctx)
	controller, err := ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&infrav1.AWSCluster{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(log, r.WatchFilterValue)).
		WithEventFilter(
			predicate.Funcs{
				// Avoid reconciling if the event triggering the reconciliation is related to incremental status updates
				// for AWSCluster resources only
				UpdateFunc: func(e event.UpdateEvent) bool {
					if e.ObjectOld.GetObjectKind().GroupVersionKind().Kind != "AWSCluster" {
						return true
					}

					oldCluster := e.ObjectOld.(*infrav1.AWSCluster).DeepCopy()
					newCluster := e.ObjectNew.(*infrav1.AWSCluster).DeepCopy()

					oldCluster.Status = infrav1.AWSClusterStatus{}
					newCluster.Status = infrav1.AWSClusterStatus{}

					oldCluster.ObjectMeta.ResourceVersion = ""
					newCluster.ObjectMeta.ResourceVersion = ""

					return !cmp.Equal(oldCluster, newCluster)
				},
			},
		).
		WithEventFilter(predicates.ResourceIsNotExternallyManaged(log)).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	return controller.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(r.requeueAWSClusterForUnpausedCluster(ctx, log)),
		predicates.ClusterUnpaused(log),
	)
}

func (r *awsClusterReconciler) requeueAWSClusterForUnpausedCluster(ctx context.Context, log logr.Logger) handler.MapFunc {
	return func(o client.Object) []ctrl.Request {
		c, ok := o.(*clusterv1.Cluster)
		if !ok {
			panic(fmt.Sprintf("Expected a Cluster but got a %T", o))
		}

		log := log.WithValues("objectMapper", "clusterToAWSCluster", "namespace", c.Namespace, "cluster", c.Name)

		// Don't handle deleted clusters
		if !c.ObjectMeta.DeletionTimestamp.IsZero() {
			log.V(4).Info("Cluster has a deletion timestamp, skipping mapping.")
			return nil
		}

		// Make sure the ref is set
		if c.Spec.InfrastructureRef == nil {
			log.V(4).Info("Cluster does not have an InfrastructureRef, skipping mapping.")
			return nil
		}

		if c.Spec.InfrastructureRef.GroupVersionKind().Kind != "AWSCluster" {
			log.V(4).Info("Cluster has an InfrastructureRef for a different type, skipping mapping.")
			return nil
		}

		awsCluster := &infrav1.AWSCluster{}
		key := types.NamespacedName{Namespace: c.Spec.InfrastructureRef.Namespace, Name: c.Spec.InfrastructureRef.Name}

		if err := r.Get(ctx, key, awsCluster); err != nil {
			log.V(4).Error(err, "Failed to get AWS cluster")
			return nil
		}

		if annotations.IsExternallyManaged(awsCluster) {
			log.V(4).Info("AWSCluster is externally managed, skipping mapping.")
			return nil
		}

		log.V(4).Info("Adding request.", "awsCluster", c.Spec.InfrastructureRef.Name)
		return []ctrl.Request{
			{
				NamespacedName: client.ObjectKey{Namespace: c.Namespace, Name: c.Spec.InfrastructureRef.Name},
			},
		}
	}
}
