/*
Copyright 2018 The Kubernetes Authors.

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

package ec2

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	"k8s.io/utils/pointer"
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha4"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/converters"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/filter"
	awslogs "sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/logs"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/userdata"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	capierrors "sigs.k8s.io/cluster-api/errors"
)

// GetRunningInstanceByTags returns the existing instance or nothing if it doesn't exist.
func (s *Service) GetRunningInstanceByTags(scope *scope.MachineScope) (*infrav1.Instance, error) {
	s.scope.V(2).Info("Looking for existing machine instance by tags")

	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			filter.EC2.VPC(s.scope.VPC().ID),
			filter.EC2.ClusterOwned(s.scope.Name()),
			filter.EC2.Name(scope.Name()),
			filter.EC2.InstanceStates(ec2.InstanceStateNamePending, ec2.InstanceStateNameRunning),
		},
	}

	out, err := s.EC2Client.DescribeInstances(input)
	switch {
	case awserrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		record.Eventf(s.scope.InfraCluster(), "FailedDescribeInstances", "Failed to describe instances by tags: %v", err)
		return nil, errors.Wrap(err, "failed to describe instances by tags")
	}

	// TODO: currently just returns the first matched instance, need to
	// better rationalize how to find the right instance to return if multiple
	// match
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			return s.SDKToInstance(inst)
		}
	}

	return nil, nil
}

// InstanceIfExists returns the existing instance or nothing if it doesn't exist.
func (s *Service) InstanceIfExists(id *string) (*infrav1.Instance, error) {
	if id == nil {
		s.scope.Info("Instance does not have an instance id")
		return nil, nil
	}

	s.scope.V(2).Info("Looking for instance by id", "instance-id", *id)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{id},
	}

	out, err := s.EC2Client.DescribeInstances(input)
	switch {
	case awserrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		record.Eventf(s.scope.InfraCluster(), "FailedDescribeInstances", "failed to describe instance %q: %v", *id, err)
		return nil, errors.Wrapf(err, "failed to describe instance: %q", *id)
	}

	if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
		return s.SDKToInstance(out.Reservations[0].Instances[0])
	}

	return nil, nil
}

// CreateInstance runs an ec2 instance.
func (s *Service) CreateInstance(scope *scope.MachineScope, userData []byte) (*infrav1.Instance, error) {
	s.scope.V(2).Info("Creating an instance for a machine")

	input := &infrav1.Instance{
		Type:              scope.AWSMachine.Spec.InstanceType,
		IAMProfile:        scope.AWSMachine.Spec.IAMInstanceProfile,
		RootVolume:        scope.AWSMachine.Spec.RootVolume,
		NonRootVolumes:    scope.AWSMachine.Spec.NonRootVolumes,
		NetworkInterfaces: scope.AWSMachine.Spec.NetworkInterfaces,
	}

	// Make sure to use the MachineScope here to get the merger of AWSCluster and AWSMachine tags
	additionalTags := scope.AdditionalTags()

	input.Tags = infrav1.Build(infrav1.BuildParams{
		ClusterName: s.scope.Name(),
		Lifecycle:   infrav1.ResourceLifecycleOwned,
		Name:        aws.String(scope.Name()),
		Role:        aws.String(scope.Role()),
		Additional:  additionalTags,
	}.WithCloudProvider(s.scope.Name()).WithMachineName(scope.Machine))

	var err error
	// Pick image from the machine configuration, or use a default one.
	if scope.AWSMachine.Spec.AMI.ID != nil { // nolint:nestif
		input.ImageID = *scope.AWSMachine.Spec.AMI.ID
	} else {
		if scope.Machine.Spec.Version == nil {
			err := errors.New("Either AWSMachine's spec.ami.id or Machine's spec.version must be defined")
			scope.SetFailureReason(capierrors.CreateMachineError)
			scope.SetFailureMessage(err)
			return nil, err
		}

		imageLookupFormat := scope.AWSMachine.Spec.ImageLookupFormat
		if imageLookupFormat == "" {
			imageLookupFormat = scope.InfraCluster.ImageLookupFormat()
		}

		imageLookupOrg := scope.AWSMachine.Spec.ImageLookupOrg
		if imageLookupOrg == "" {
			imageLookupOrg = scope.InfraCluster.ImageLookupOrg()
		}

		imageLookupBaseOS := scope.AWSMachine.Spec.ImageLookupBaseOS
		if imageLookupBaseOS == "" {
			imageLookupBaseOS = scope.InfraCluster.ImageLookupBaseOS()
		}

		if scope.IsEKSManaged() && imageLookupFormat == "" && imageLookupOrg == "" && imageLookupBaseOS == "" {
			input.ImageID, err = s.eksAMILookup(*scope.Machine.Spec.Version, scope.AWSMachine.Spec.AMI.EKSOptimizedLookupType)
			if err != nil {
				return nil, err
			}
		} else {
			input.ImageID, err = s.defaultAMIIDLookup(imageLookupFormat, imageLookupOrg, imageLookupBaseOS, *scope.Machine.Spec.Version)
			if err != nil {
				return nil, err
			}
		}
	}

	subnetID, err := s.findSubnet(scope)
	if err != nil {
		return nil, err
	}
	input.SubnetID = subnetID

	if !scope.IsExternallyManaged() && !scope.IsEKSManaged() && s.scope.Network().APIServerELB.DNSName == "" {
		record.Eventf(s.scope.InfraCluster(), "FailedCreateInstance", "Failed to run controlplane, APIServer ELB not available")

		return nil, awserrors.NewFailedDependency("failed to run controlplane, APIServer ELB not available")
	}
	if !scope.UserDataIsUncompressed() {
		userData, err = userdata.GzipBytes(userData)
		if err != nil {
			return nil, errors.New("failed to gzip userdata")
		}
	}

	input.UserData = pointer.StringPtr(base64.StdEncoding.EncodeToString(userData))

	// Set security groups.
	ids, err := s.GetCoreSecurityGroups(scope)
	if err != nil {
		return nil, err
	}
	input.SecurityGroupIDs = append(input.SecurityGroupIDs, ids...)

	// If SSHKeyName WAS NOT provided in the AWSMachine Spec, fallback to the value provided in the AWSCluster Spec.
	// If a value was not provided in the AWSCluster Spec, then use the defaultSSHKeyName
	// Note that:
	// - a nil AWSMachine.Spec.SSHKeyName value means use the AWSCluster.Spec.SSHKeyName SSH key name value
	// - nil values for both AWSCluster.Spec.SSHKeyName and AWSMachine.Spec.SSHKeyName means use the default SSH key name value
	// - an empty string means do not set an SSH key name at all
	// - otherwise use the value specified in either AWSMachine or AWSCluster
	var prioritizedSSHKeyName string
	switch {
	case scope.AWSMachine.Spec.SSHKeyName != nil:
		// prefer AWSMachine.Spec.SSHKeyName if it is defined
		prioritizedSSHKeyName = *scope.AWSMachine.Spec.SSHKeyName
	case scope.InfraCluster.SSHKeyName() != nil:
		// fallback to AWSCluster.Spec.SSHKeyName if it is defined
		prioritizedSSHKeyName = *scope.InfraCluster.SSHKeyName()
	default:
		if !scope.IsExternallyManaged() {
			prioritizedSSHKeyName = defaultSSHKeyName
		}
	}

	// Only set input.SSHKeyName if the user did not explicitly request no ssh key be set (explicitly setting "" on either the Machine or related Cluster)
	if prioritizedSSHKeyName != "" {
		input.SSHKeyName = aws.String(prioritizedSSHKeyName)
	}

	input.SpotMarketOptions = scope.AWSMachine.Spec.SpotMarketOptions

	input.Tenancy = scope.AWSMachine.Spec.Tenancy

	s.scope.V(2).Info("Running instance", "machine-role", scope.Role())
	out, err := s.runInstance(scope.Role(), input)
	if err != nil {
		// Only record the failure event if the error is not related to failed dependencies.
		// This is to avoid spamming failure events since the machine will be requeued by the actuator.
		if !awserrors.IsFailedDependency(errors.Cause(err)) {
			record.Warnf(scope.AWSMachine, "FailedCreate", "Failed to create instance: %v", err)
		}
		return nil, err
	}

	if len(input.NetworkInterfaces) > 0 {
		for _, id := range input.NetworkInterfaces {
			s.scope.V(2).Info("Attaching security groups to provided network interface", "groups", input.SecurityGroupIDs, "interface", id)
			if err := s.attachSecurityGroupsToNetworkInterface(input.SecurityGroupIDs, id); err != nil {
				return nil, err
			}
		}
	}

	record.Eventf(scope.AWSMachine, "SuccessfulCreate", "Created new %s instance with id %q", scope.Role(), out.ID)
	return out, nil
}

// findSubnet attempts to retrieve a subnet ID in the following order:
// - subnetID specified in machine configuration,
// - subnet based on filters in machine configuration
// - subnet based on the availability zone specified,
// - default to the first private subnet available.
func (s *Service) findSubnet(scope *scope.MachineScope) (string, error) {
	// Check Machine.Spec.FailureDomain first as it's used by KubeadmControlPlane to spread machines across failure domains.
	failureDomain := scope.Machine.Spec.FailureDomain
	if failureDomain == nil {
		failureDomain = scope.AWSMachine.Spec.FailureDomain
	}

	switch {
	case scope.AWSMachine.Spec.Subnet != nil && scope.AWSMachine.Spec.Subnet.ID != nil:
		if failureDomain != nil {
			subnet := s.scope.Subnets().FindByID(*scope.AWSMachine.Spec.Subnet.ID)
			if subnet == nil {
				record.Warnf(scope.AWSMachine, "FailedCreate",
					"Failed to create instance: subnet with id %q not found", aws.StringValue(scope.AWSMachine.Spec.Subnet.ID))
				return "", awserrors.NewFailedDependency(
					fmt.Sprintf("failed to run machine %q, subnet with id %q not found",
						scope.Name(),
						aws.StringValue(scope.AWSMachine.Spec.Subnet.ID),
					),
				)
			}

			if subnet.AvailabilityZone != *failureDomain {
				record.Warnf(scope.AWSMachine, "FailedCreate",
					"Failed to create instance: subnet's availability zone %q does not match with the failure domain %q",
					subnet.AvailabilityZone,
					*failureDomain)
				return "", awserrors.NewFailedDependency(
					fmt.Sprintf("failed to run machine %q, subnet's availability zone %q does not match with the failure domain %q",
						scope.Name(),
						subnet.AvailabilityZone,
						*failureDomain,
					),
				)
			}
		}
		return *scope.AWSMachine.Spec.Subnet.ID, nil
	case scope.AWSMachine.Spec.Subnet != nil && scope.AWSMachine.Spec.Subnet.Filters != nil:
		criteria := []*ec2.Filter{
			filter.EC2.SubnetStates(ec2.SubnetStatePending, ec2.SubnetStateAvailable),
		}
		if !scope.IsExternallyManaged() {
			criteria = append(criteria, filter.EC2.VPC(s.scope.VPC().ID))
		}
		if failureDomain != nil {
			criteria = append(criteria, filter.EC2.AvailabilityZone(*failureDomain))
		}
		for _, f := range scope.AWSMachine.Spec.Subnet.Filters {
			criteria = append(criteria, &ec2.Filter{Name: aws.String(f.Name), Values: aws.StringSlice(f.Values)})
		}
		subnets, err := s.getFilteredSubnets(criteria...)
		if err != nil {
			return "", errors.Wrapf(err, "failed to filter subnets for criteria %q", criteria)
		}
		if len(subnets) == 0 {
			record.Warnf(scope.AWSMachine, "FailedCreate",
				"Failed to create instance: no subnets available matching filters %q", scope.AWSMachine.Spec.Subnet.Filters)
			return "", awserrors.NewFailedDependency(
				fmt.Sprintf("failed to run machine %q, no subnets available matching filters %q",
					scope.Name(),
					scope.AWSMachine.Spec.Subnet.Filters,
				),
			)
		}
		return *subnets[0].SubnetId, nil

	case failureDomain != nil:
		subnets := s.scope.Subnets().FilterPrivate().FilterByZone(*failureDomain)
		if len(subnets) == 0 {
			record.Warnf(scope.AWSMachine, "FailedCreate",
				"Failed to create instance: no subnets available in availability zone %q", *failureDomain)

			return "", awserrors.NewFailedDependency(
				fmt.Sprintf("failed to run machine %q, no subnets available in availability zone %q",
					scope.Name(),
					*failureDomain,
				),
			)
		}
		return subnets[0].ID, nil

		// TODO(vincepri): Define a tag that would allow to pick a preferred subnet in an AZ when working
		// with control plane machines.

	default:
		sns := s.scope.Subnets().FilterPrivate()
		if len(sns) == 0 {
			record.Eventf(s.scope.InfraCluster(), "FailedCreateInstance", "Failed to run machine %q, no subnets available", scope.Name())
			return "", awserrors.NewFailedDependency(fmt.Sprintf("failed to run machine %q, no subnets available", scope.Name()))
		}
		return sns[0].ID, nil
	}
}

// getFilteredSubnets fetches subnets filtered based on the criteria passed.
func (s *Service) getFilteredSubnets(criteria ...*ec2.Filter) ([]*ec2.Subnet, error) {
	out, err := s.EC2Client.DescribeSubnets(&ec2.DescribeSubnetsInput{Filters: criteria})
	if err != nil {
		return nil, err
	}
	return out.Subnets, nil
}

// GetCoreSecurityGroups looks up the security group IDs managed by this actuator
// They are considered "core" to its proper functioning.
func (s *Service) GetCoreSecurityGroups(scope *scope.MachineScope) ([]string, error) {
	if scope.IsExternallyManaged() {
		return nil, nil
	}

	// These are common across both controlplane and node machines
	sgRoles := []infrav1.SecurityGroupRole{
		infrav1.SecurityGroupNode,
	}

	if !scope.IsEKSManaged() {
		sgRoles = append(sgRoles, infrav1.SecurityGroupLB)
	}

	switch scope.Role() {
	case "node":
		// Just the common security groups above
		if scope.IsEKSManaged() {
			sgRoles = append(sgRoles, infrav1.SecurityGroupEKSNodeAdditional)
		}
	case "control-plane":
		sgRoles = append(sgRoles, infrav1.SecurityGroupControlPlane)
	default:
		return nil, errors.Errorf("Unknown node role %q", scope.Role())
	}
	ids := make([]string, 0, len(sgRoles))
	for _, sg := range sgRoles {
		if _, ok := s.scope.SecurityGroups()[sg]; !ok {
			return nil, awserrors.NewFailedDependency(fmt.Sprintf("%s security group not available", sg))
		}
		ids = append(ids, s.scope.SecurityGroups()[sg].ID)
	}
	return ids, nil
}

// GetCoreNodeSecurityGroups looks up the security group IDs managed by this actuator
// They are considered "core" to its proper functioning.
func (s *Service) GetCoreNodeSecurityGroups(scope *scope.MachinePoolScope) ([]string, error) {
	// These are common across both controlplane and node machines
	sgRoles := []infrav1.SecurityGroupRole{
		infrav1.SecurityGroupNode,
	}

	if !scope.IsEKSManaged() {
		sgRoles = append(sgRoles, infrav1.SecurityGroupLB)
	} else {
		sgRoles = append(sgRoles, infrav1.SecurityGroupEKSNodeAdditional)
	}

	ids := make([]string, 0, len(sgRoles))
	for _, sg := range sgRoles {
		if _, ok := s.scope.SecurityGroups()[sg]; !ok {
			return nil, awserrors.NewFailedDependency(
				fmt.Sprintf("%s security group not available", sg),
			)
		}
		ids = append(ids, s.scope.SecurityGroups()[sg].ID)
	}
	return ids, nil
}

// TerminateInstance terminates an EC2 instance.
// Returns nil on success, error in all other cases.
func (s *Service) TerminateInstance(instanceID string) error {
	s.scope.V(2).Info("Attempting to terminate instance", "instance-id", instanceID)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	}

	if _, err := s.EC2Client.TerminateInstances(input); err != nil {
		return errors.Wrapf(err, "failed to terminate instance with id %q", instanceID)
	}

	s.scope.V(2).Info("Terminated instance", "instance-id", instanceID)
	return nil
}

// TerminateInstanceAndWait terminates and waits
// for an EC2 instance to terminate.
func (s *Service) TerminateInstanceAndWait(instanceID string) error {
	if err := s.TerminateInstance(instanceID); err != nil {
		return err
	}

	s.scope.V(2).Info("Waiting for EC2 instance to terminate", "instance-id", instanceID)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	}

	if err := s.EC2Client.WaitUntilInstanceTerminated(input); err != nil {
		return errors.Wrapf(err, "failed to wait for instance %q termination", instanceID)
	}

	return nil
}

func (s *Service) runInstance(role string, i *infrav1.Instance) (*infrav1.Instance, error) {
	input := &ec2.RunInstancesInput{
		InstanceType: aws.String(i.Type),
		ImageId:      aws.String(i.ImageID),
		KeyName:      i.SSHKeyName,
		EbsOptimized: i.EBSOptimized,
		MaxCount:     aws.Int64(1),
		MinCount:     aws.Int64(1),
		UserData:     i.UserData,
	}

	s.scope.V(2).Info("userData size", "bytes", len(*i.UserData), "role", role)

	if len(i.NetworkInterfaces) > 0 {
		netInterfaces := make([]*ec2.InstanceNetworkInterfaceSpecification, 0, len(i.NetworkInterfaces))

		for index, id := range i.NetworkInterfaces {
			netInterfaces = append(netInterfaces, &ec2.InstanceNetworkInterfaceSpecification{
				NetworkInterfaceId: aws.String(id),
				DeviceIndex:        aws.Int64(int64(index)),
			})
		}

		input.NetworkInterfaces = netInterfaces
	} else {
		input.SubnetId = aws.String(i.SubnetID)

		if len(i.SecurityGroupIDs) > 0 {
			input.SecurityGroupIds = aws.StringSlice(i.SecurityGroupIDs)
		}
	}

	if i.IAMProfile != "" {
		input.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{
			Name: aws.String(i.IAMProfile),
		}
	}

	blockdeviceMappings := []*ec2.BlockDeviceMapping{}

	if i.RootVolume != nil {
		rootDeviceName, err := s.checkRootVolume(i.RootVolume, i.ImageID)
		if err != nil {
			return nil, err
		}

		ebsRootDevice := &ec2.EbsBlockDevice{
			DeleteOnTermination: aws.Bool(true),
			VolumeSize:          aws.Int64(i.RootVolume.Size),
			Encrypted:           aws.Bool(i.RootVolume.Encrypted),
		}

		if i.RootVolume.IOPS != 0 {
			ebsRootDevice.Iops = aws.Int64(i.RootVolume.IOPS)
		}

		if i.RootVolume.EncryptionKey != "" {
			ebsRootDevice.Encrypted = aws.Bool(true)
			ebsRootDevice.KmsKeyId = aws.String(i.RootVolume.EncryptionKey)
		}

		if i.RootVolume.Type != "" {
			ebsRootDevice.VolumeType = aws.String(i.RootVolume.Type)
		}

		blockdeviceMappings = append(blockdeviceMappings, &ec2.BlockDeviceMapping{
			DeviceName: rootDeviceName,
			Ebs:        ebsRootDevice,
		})
	}

	for vi := range i.NonRootVolumes {
		nonRootVolume := i.NonRootVolumes[vi]

		if nonRootVolume.DeviceName == "" {
			return nil, errors.Errorf("non root volume should have device name specified")
		}

		ebsDevice := &ec2.EbsBlockDevice{
			DeleteOnTermination: aws.Bool(true),
			VolumeSize:          aws.Int64(nonRootVolume.Size),
			Encrypted:           aws.Bool(nonRootVolume.Encrypted),
		}

		if nonRootVolume.IOPS != 0 {
			ebsDevice.Iops = aws.Int64(nonRootVolume.IOPS)
		}

		if nonRootVolume.EncryptionKey != "" {
			ebsDevice.Encrypted = aws.Bool(true)
			ebsDevice.KmsKeyId = aws.String(nonRootVolume.EncryptionKey)
		}

		if nonRootVolume.Type != "" {
			ebsDevice.VolumeType = aws.String(nonRootVolume.Type)
		}

		blockdeviceMappings = append(blockdeviceMappings, &ec2.BlockDeviceMapping{
			DeviceName: &nonRootVolume.DeviceName,
			Ebs:        ebsDevice,
		})
	}

	if len(blockdeviceMappings) != 0 {
		input.BlockDeviceMappings = blockdeviceMappings
	}

	if len(i.Tags) > 0 {
		spec := &ec2.TagSpecification{ResourceType: aws.String(ec2.ResourceTypeInstance)}
		// We need to sort keys for tests to work
		keys := make([]string, 0, len(i.Tags))
		for k := range i.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			spec.Tags = append(spec.Tags, &ec2.Tag{
				Key:   aws.String(key),
				Value: aws.String(i.Tags[key]),
			})
		}

		input.TagSpecifications = append(input.TagSpecifications, spec)
	}

	input.InstanceMarketOptions = getInstanceMarketOptionsRequest(i.SpotMarketOptions)

	if i.Tenancy != "" {
		input.Placement = &ec2.Placement{
			Tenancy: &i.Tenancy,
		}
	}

	out, err := s.EC2Client.RunInstances(input)
	if err != nil {
		return nil, errors.Wrap(err, "failed to run instance")
	}

	if len(out.Instances) == 0 {
		return nil, errors.Errorf("no instance returned for reservation %v", out.GoString())
	}

	waitTimeout := 1 * time.Minute
	s.scope.V(2).Info("Waiting for instance to be in running state", "instance-id", *out.Instances[0].InstanceId, "timeout", waitTimeout.String())
	ctx, cancel := context.WithTimeout(aws.BackgroundContext(), waitTimeout)
	defer cancel()

	if err := s.EC2Client.WaitUntilInstanceRunningWithContext(
		ctx,
		&ec2.DescribeInstancesInput{InstanceIds: []*string{out.Instances[0].InstanceId}},
		request.WithWaiterLogger(awslogs.NewWrapLogr(s.scope)),
	); err != nil {
		s.scope.V(2).Info("Could not determine if Machine is running. Machine state might be unavailable until next renconciliation.")
	}

	return s.SDKToInstance(out.Instances[0])
}

// GetInstanceSecurityGroups returns a map from ENI id to the security groups applied to that ENI
// While some security group operations take place at the "instance" level, these are in fact an API convenience for manipulating the first ("primary") ENI's properties.
func (s *Service) GetInstanceSecurityGroups(instanceID string) (map[string][]string, error) {
	enis, err := s.getInstanceENIs(instanceID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get ENIs for instance %q", instanceID)
	}

	out := make(map[string][]string)
	for _, eni := range enis {
		var groups []string
		for _, group := range eni.Groups {
			groups = append(groups, aws.StringValue(group.GroupId))
		}
		out[aws.StringValue(eni.NetworkInterfaceId)] = groups
	}
	return out, nil
}

// UpdateInstanceSecurityGroups modifies the security groups of the given
// EC2 instance.
func (s *Service) UpdateInstanceSecurityGroups(instanceID string, ids []string) error {
	s.scope.V(2).Info("Attempting to update security groups on instance", "instance-id", instanceID)

	enis, err := s.getInstanceENIs(instanceID)
	if err != nil {
		return errors.Wrapf(err, "failed to get ENIs for instance %q", instanceID)
	}

	s.scope.V(3).Info("Found ENIs on instance", "number-of-enis", len(enis), "instance-id", instanceID)

	for _, eni := range enis {
		if err := s.attachSecurityGroupsToNetworkInterface(ids, aws.StringValue(eni.NetworkInterfaceId)); err != nil {
			return errors.Wrapf(err, "failed to modify network interfaces on instance %q", instanceID)
		}
	}

	return nil
}

// UpdateResourceTags updates the tags for an instance.
// This will be called if there is anything to create (update) or delete.
// We may not always have to perform each action, so we check what we're
// receiving to avoid calling AWS if we don't need to.
func (s *Service) UpdateResourceTags(resourceID *string, create, remove map[string]string) error {
	s.scope.V(2).Info("Attempting to update tags on resource", "resource-id", *resourceID)

	// If we have anything to create or update
	if len(create) > 0 {
		s.scope.V(2).Info("Attempting to create tags on resource", "resource-id", *resourceID)

		// Convert our create map into an array of *ec2.Tag
		createTagsInput := converters.MapToTags(create)

		// Create the CreateTags input.
		input := &ec2.CreateTagsInput{
			Resources: []*string{resourceID},
			Tags:      createTagsInput,
		}

		// Create/Update tags in AWS.
		if _, err := s.EC2Client.CreateTags(input); err != nil {
			return errors.Wrapf(err, "failed to create tags for resource %q: %+v", *resourceID, create)
		}
	}

	// If we have anything to remove
	if len(remove) > 0 {
		s.scope.V(2).Info("Attempting to delete tags on resource", "resource-id", *resourceID)

		// Convert our remove map into an array of *ec2.Tag
		removeTagsInput := converters.MapToTags(remove)

		// Create the DeleteTags input
		input := &ec2.DeleteTagsInput{
			Resources: []*string{resourceID},
			Tags:      removeTagsInput,
		}

		// Delete tags in AWS.
		if _, err := s.EC2Client.DeleteTags(input); err != nil {
			return errors.Wrapf(err, "failed to delete tags for resource %q: %v", *resourceID, remove)
		}
	}

	return nil
}

func (s *Service) getInstanceENIs(instanceID string) ([]*ec2.NetworkInterface, error) {
	input := &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("attachment.instance-id"),
				Values: []*string{aws.String(instanceID)},
			},
		},
	}

	output, err := s.EC2Client.DescribeNetworkInterfaces(input)
	if err != nil {
		return nil, err
	}

	return output.NetworkInterfaces, nil
}

func (s *Service) getImageRootDevice(imageID string) (*string, error) {
	input := &ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(imageID)},
	}

	output, err := s.EC2Client.DescribeImages(input)
	if err != nil {
		return nil, err
	}

	if len(output.Images) == 0 {
		return nil, errors.Errorf("no images returned when looking up ID %q", imageID)
	}

	return output.Images[0].RootDeviceName, nil
}

func (s *Service) getImageSnapshotSize(imageID string) (*int64, error) {
	input := &ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(imageID)},
	}

	output, err := s.EC2Client.DescribeImages(input)
	if err != nil {
		return nil, err
	}

	if len(output.Images) == 0 {
		return nil, errors.Errorf("no images returned when looking up ID %q", imageID)
	}

	return output.Images[0].BlockDeviceMappings[0].Ebs.VolumeSize, nil
}

// SDKToInstance converts an AWS EC2 SDK instance to the CAPA instance type.
// SDKToInstance populates all instance fields except for rootVolumeSize,
// because EC2.DescribeInstances does not return the size of storage devices. An
// additional call to EC2 is required to get this value.
func (s *Service) SDKToInstance(v *ec2.Instance) (*infrav1.Instance, error) {
	i := &infrav1.Instance{
		ID:           aws.StringValue(v.InstanceId),
		State:        infrav1.InstanceState(*v.State.Name),
		Type:         aws.StringValue(v.InstanceType),
		SubnetID:     aws.StringValue(v.SubnetId),
		ImageID:      aws.StringValue(v.ImageId),
		SSHKeyName:   v.KeyName,
		PrivateIP:    v.PrivateIpAddress,
		PublicIP:     v.PublicIpAddress,
		ENASupport:   v.EnaSupport,
		EBSOptimized: v.EbsOptimized,
	}

	// Extract IAM Instance Profile name from ARN
	// TODO: Handle this comparison more safely, perhaps by querying IAM for the
	// instance profile ARN and comparing to the ARN returned by EC2
	if v.IamInstanceProfile != nil && v.IamInstanceProfile.Arn != nil {
		split := strings.Split(aws.StringValue(v.IamInstanceProfile.Arn), "instance-profile/")
		if len(split) > 1 && split[1] != "" {
			i.IAMProfile = split[1]
		}
	}

	for _, sg := range v.SecurityGroups {
		i.SecurityGroupIDs = append(i.SecurityGroupIDs, *sg.GroupId)
	}

	if len(v.Tags) > 0 {
		i.Tags = converters.TagsToMap(v.Tags)
	}

	i.Addresses = s.getInstanceAddresses(v)

	i.AvailabilityZone = aws.StringValue(v.Placement.AvailabilityZone)

	for _, volume := range v.BlockDeviceMappings {
		i.VolumeIDs = append(i.VolumeIDs, *volume.Ebs.VolumeId)
	}

	return i, nil
}

func (s *Service) getInstanceAddresses(instance *ec2.Instance) []clusterv1.MachineAddress {
	addresses := []clusterv1.MachineAddress{}
	for _, eni := range instance.NetworkInterfaces {
		privateDNSAddress := clusterv1.MachineAddress{
			Type:    clusterv1.MachineInternalDNS,
			Address: aws.StringValue(eni.PrivateDnsName),
		}
		privateIPAddress := clusterv1.MachineAddress{
			Type:    clusterv1.MachineInternalIP,
			Address: aws.StringValue(eni.PrivateIpAddress),
		}
		addresses = append(addresses, privateDNSAddress, privateIPAddress)

		// An elastic IP is attached if association is non nil pointer
		if eni.Association != nil {
			publicDNSAddress := clusterv1.MachineAddress{
				Type:    clusterv1.MachineExternalDNS,
				Address: aws.StringValue(eni.Association.PublicDnsName),
			}
			publicIPAddress := clusterv1.MachineAddress{
				Type:    clusterv1.MachineExternalIP,
				Address: aws.StringValue(eni.Association.PublicIp),
			}
			addresses = append(addresses, publicDNSAddress, publicIPAddress)
		}
	}
	return addresses
}

func (s *Service) getNetworkInterfaceSecurityGroups(interfaceID string) ([]string, error) {
	input := &ec2.DescribeNetworkInterfaceAttributeInput{
		Attribute:          aws.String("groupSet"),
		NetworkInterfaceId: aws.String(interfaceID),
	}

	output, err := s.EC2Client.DescribeNetworkInterfaceAttribute(input)
	if err != nil {
		return nil, err
	}

	groups := make([]string, len(output.Groups))
	for i := range output.Groups {
		groups[i] = aws.StringValue(output.Groups[i].GroupId)
	}

	return groups, nil
}

func (s *Service) attachSecurityGroupsToNetworkInterface(groups []string, interfaceID string) error {
	existingGroups, err := s.getNetworkInterfaceSecurityGroups(interfaceID)
	if err != nil {
		return errors.Wrapf(err, "failed to look up network interface security groups: %+v", err)
	}

	totalGroups := make([]string, len(existingGroups))
	copy(totalGroups, existingGroups)

	for _, group := range groups {
		if !containsGroup(existingGroups, group) {
			totalGroups = append(totalGroups, group)
		}
	}

	// no new groups to attach
	if len(existingGroups) == len(totalGroups) {
		return nil
	}

	s.scope.Info("Updating security groups", "groups", totalGroups)

	input := &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(interfaceID),
		Groups:             aws.StringSlice(totalGroups),
	}

	if _, err := s.EC2Client.ModifyNetworkInterfaceAttribute(input); err != nil {
		return errors.Wrapf(err, "failed to modify interface %q to have security groups %v", interfaceID, totalGroups)
	}
	return nil
}

// DetachSecurityGroupsFromNetworkInterface looks up an ENI by interfaceID and
// detaches a list of Security Groups from that ENI.
func (s *Service) DetachSecurityGroupsFromNetworkInterface(groups []string, interfaceID string) error {
	existingGroups, err := s.getNetworkInterfaceSecurityGroups(interfaceID)
	if err != nil {
		return errors.Wrapf(err, "failed to look up network interface security groups")
	}

	remainingGroups := existingGroups
	for _, group := range groups {
		remainingGroups = filterGroups(remainingGroups, group)
	}

	input := &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(interfaceID),
		Groups:             aws.StringSlice(remainingGroups),
	}

	if _, err := s.EC2Client.ModifyNetworkInterfaceAttribute(input); err != nil {
		return errors.Wrapf(err, "failed to modify interface %q", interfaceID)
	}
	return nil
}

// checkRootVolume checks the input root volume options against the requested AMI's defaults
// and returns the AMI's root device name.
func (s *Service) checkRootVolume(rootVolume *infrav1.Volume, imageID string) (*string, error) {
	rootDeviceName, err := s.getImageRootDevice(imageID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get root volume from image %q", imageID)
	}

	snapshotSize, err := s.getImageSnapshotSize(imageID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get root volume from image %q", imageID)
	}

	if rootVolume.Size < *snapshotSize {
		return nil, errors.Errorf("root volume size (%d) must be greater than or equal to snapshot size (%d)", rootVolume.Size, *snapshotSize)
	}

	return rootDeviceName, nil
}

// filterGroups filters a list for a string.
func filterGroups(list []string, strToFilter string) (newList []string) {
	for _, item := range list {
		if item != strToFilter {
			newList = append(newList, item)
		}
	}
	return
}

// containsGroup returns true if a list contains a string.
func containsGroup(list []string, strToSearch string) bool {
	for _, item := range list {
		if item == strToSearch {
			return true
		}
	}
	return false
}

func getInstanceMarketOptionsRequest(spotMarketOptions *infrav1.SpotMarketOptions) *ec2.InstanceMarketOptionsRequest {
	if spotMarketOptions == nil {
		// Instance is not a Spot instance
		return nil
	}

	// Set required values for Spot instances
	spotOptions := &ec2.SpotMarketOptions{}

	// The following two options ensure that:
	// - If an instance is interrupted, it is terminated rather than hibernating or stopping
	// - No replacement instance will be created if the instance is interrupted
	// - If the spot request cannot immediately be fulfilled, it will not be created
	// This behaviour should satisfy the 1:1 mapping of Machines to Instances as
	// assumed by the Cluster API.
	spotOptions.SetInstanceInterruptionBehavior(ec2.InstanceInterruptionBehaviorTerminate)
	spotOptions.SetSpotInstanceType(ec2.SpotInstanceTypeOneTime)

	maxPrice := spotMarketOptions.MaxPrice
	if maxPrice != nil && *maxPrice != "" {
		spotOptions.SetMaxPrice(*maxPrice)
	}

	instanceMarketOptionsRequest := &ec2.InstanceMarketOptionsRequest{}
	instanceMarketOptionsRequest.SetMarketType(ec2.MarketTypeSpot)
	instanceMarketOptionsRequest.SetSpotOptions(spotOptions)

	return instanceMarketOptionsRequest
}

// GetFilteredSecurityGroupID get security group ID using filters.
func (s *Service) GetFilteredSecurityGroupID(securityGroup infrav1.AWSResourceReference) (string, error) {
	if securityGroup.Filters == nil {
		return "", nil
	}

	filters := []*ec2.Filter{}
	for _, f := range securityGroup.Filters {
		filters = append(filters, &ec2.Filter{Name: aws.String(f.Name), Values: aws.StringSlice(f.Values)})
	}

	sgs, err := s.EC2Client.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{Filters: filters})
	if err != nil {
		return "", err
	}

	if len(sgs.SecurityGroups) == 0 {
		return "", fmt.Errorf("failed to find security group matching filters: %q, reason: %w", filters, err)
	}

	return *sgs.SecurityGroups[0].GroupId, nil
}
