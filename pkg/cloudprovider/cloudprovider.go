/*
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

package cloudprovider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter/pkg/apis"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils"

	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/resources"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter/pkg/cloudprovider/amifamily"
	awscontext "github.com/aws/karpenter/pkg/context"

	coreapis "github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

const (
	// MaxInstanceTypes defines the number of instance type options to pass to CreateFleet
	MaxInstanceTypes = 60
)

func init() {
	v1alpha5.NormalizedLabels = lo.Assign(v1alpha5.NormalizedLabels, map[string]string{"topology.ebs.csi.aws.com/zone": v1.LabelTopologyZone})
	coreapis.Settings = append(coreapis.Settings, apis.Settings...)
}

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	instanceTypeProvider *InstanceTypeProvider
	instanceProvider     *InstanceProvider
	kubeClient           client.Client
	amiProvider          *amifamily.AMIProvider
}

func New(ctx awscontext.Context) *CloudProvider {
	instanceTypeProvider := NewInstanceTypeProvider(ctx.Session, ctx.EC2API, ctx.SubnetProvider, ctx.UnavailableOfferingsCache, ctx.PricingProvider)
	return &CloudProvider{
		kubeClient:           ctx.KubeClient,
		instanceTypeProvider: instanceTypeProvider,
		amiProvider:          ctx.AMIProvider,
		instanceProvider: NewInstanceProvider(
			ctx,
			aws.StringValue(ctx.Session.Config.Region),
			ctx.EC2API,
			ctx.UnavailableOfferingsCache,
			instanceTypeProvider,
			ctx.SubnetProvider,
			ctx.LaunchTemplateProvider,
		),
	}
}

// Create a machine given the constraints.
func (c *CloudProvider) Create(ctx context.Context, machine *v1alpha5.Machine) (*v1alpha5.Machine, error) {
	nodeTemplate, err := c.resolveNodeTemplate(ctx, []byte(machine.
		Annotations[v1alpha5.ProviderCompatabilityAnnotationKey]), machine.
		Spec.MachineTemplateRef)
	if err != nil {
		return nil, fmt.Errorf("resolving node template, %w", err)
	}
	instanceTypes, err := c.resolveInstanceTypes(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("resolving instance types, %w", err)
	}
	if len(instanceTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all requested instance types were unavailable during launch"))
	}
	instance, err := c.instanceProvider.Create(ctx, nodeTemplate, machine, instanceTypes)
	if err != nil {
		return nil, fmt.Errorf("creating instance, %w", err)
	}
	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == aws.StringValue(instance.InstanceType)
	})
	return c.instanceToMachine(ctx, instance, instanceType), nil
}

// Link adds a tag to the cloudprovider machine to tell the cloudprovider that it's now owned by a Machine
func (c *CloudProvider) Link(ctx context.Context, machine *v1alpha5.Machine) error {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("machine", machine.Name))
	id, err := utils.ParseInstanceID(machine.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("id", id))
	return c.instanceProvider.Link(ctx, id)
}

func (c *CloudProvider) List(ctx context.Context) ([]*v1alpha5.Machine, error) {
	instances, err := c.instanceProvider.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing instances, %w", err)
	}
	var machines []*v1alpha5.Machine
	for _, instance := range instances {
		instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
		if err != nil {
			return nil, fmt.Errorf("resolving instance type, %w", err)
		}
		machines = append(machines, c.instanceToMachine(ctx, instance, instanceType))
	}
	return machines, nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*v1alpha5.Machine, error) {
	id, err := utils.ParseInstanceID(providerID)
	if err != nil {
		return nil, fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("id", id))
	instance, err := c.instanceProvider.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting instance, %w", err)
	}
	instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("resolving instance type, %w", err)
	}
	return c.instanceToMachine(ctx, instance, instanceType), nil
}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	if err := c.instanceTypeProvider.LivenessProbe(req); err != nil {
		return err
	}
	return nil
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, provisioner *v1alpha5.Provisioner) ([]*cloudprovider.InstanceType, error) {
	var rawProvider []byte
	if provisioner.Spec.Provider != nil {
		rawProvider = provisioner.Spec.Provider.Raw
	}
	nodeTemplate, err := c.resolveNodeTemplate(ctx, rawProvider, provisioner.Spec.ProviderRef)
	if err != nil {
		return nil, err
	}
	// TODO, break this coupling
	instanceTypes, err := c.instanceTypeProvider.List(ctx, provisioner.Spec.KubeletConfiguration, nodeTemplate)
	if err != nil {
		return nil, err
	}
	return instanceTypes, nil
}

func (c *CloudProvider) Delete(ctx context.Context, machine *v1alpha5.Machine) error {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("machine", machine.Name))
	id, err := utils.ParseInstanceID(machine.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("id", id))
	return c.instanceProvider.Delete(ctx, id)
}

func (c *CloudProvider) IsMachineDrifted(ctx context.Context, machine *v1alpha5.Machine) (bool, error) {
	// Not needed when GetInstanceTypes removes provisioner dependency
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: machine.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		return false, client.IgnoreNotFound(fmt.Errorf("getting provisioner, %w", err))
	}
	if provisioner.Spec.ProviderRef == nil {
		return false, nil
	}
	nodeTemplate, err := c.resolveNodeTemplate(ctx, nil, provisioner.Spec.ProviderRef)
	if err != nil {
		return false, client.IgnoreNotFound(fmt.Errorf("resolving node template, %w", err))
	}
	amiDrifted, err := c.isAMIDrifted(ctx, machine, provisioner, nodeTemplate)
	if err != nil {
		return false, err
	}
	return amiDrifted, nil
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "aws"
}

func (c *CloudProvider) isAMIDrifted(ctx context.Context, machine *v1alpha5.Machine, provisioner *v1alpha5.Provisioner, nodeTemplate *v1alpha1.AWSNodeTemplate) (bool, error) {
	instanceTypes, err := c.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return false, fmt.Errorf("getting instanceTypes, %w", err)
	}
	nodeInstanceType, found := lo.Find(instanceTypes, func(instType *cloudprovider.InstanceType) bool {
		return instType.Name == machine.Labels[v1.LabelInstanceTypeStable]
	})
	if !found {
		return false, fmt.Errorf(`finding node instance type "%s"`, machine.Labels[v1.LabelInstanceTypeStable])
	}
	if nodeTemplate.Spec.LaunchTemplateName != nil {
		return false, nil
	}
	amis, err := c.amiProvider.Get(ctx, nodeTemplate, []*cloudprovider.InstanceType{nodeInstanceType},
		amifamily.GetAMIFamily(nodeTemplate.Spec.AMIFamily, &amifamily.Options{}))
	if err != nil {
		return false, fmt.Errorf("getting amis, %w", err)
	}
	// Get InstanceID to fetch from EC2
	instanceID, err := utils.ParseInstanceID(machine.Status.ProviderID)
	if err != nil {
		return false, err
	}
	instance, err := c.instanceProvider.Get(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("getting instance, %w", err)
	}
	return !lo.Contains(lo.Keys(amis), *instance.ImageId), nil
}

func (c *CloudProvider) resolveNodeTemplate(ctx context.Context, raw []byte, objRef *v1alpha5.ProviderRef) (*v1alpha1.AWSNodeTemplate, error) {
	nodeTemplate := &v1alpha1.AWSNodeTemplate{}
	if objRef != nil {
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: objRef.Name}, nodeTemplate); err != nil {
			return nil, fmt.Errorf("getting providerRef, %w", err)
		}
		return nodeTemplate, nil
	}
	aws, err := v1alpha1.DeserializeProvider(raw)
	if err != nil {
		return nil, err
	}
	nodeTemplate.Spec.AWS = lo.FromPtr(aws)
	return nodeTemplate, nil
}

func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, machine *v1alpha5.Machine) ([]*cloudprovider.InstanceType, error) {
	provisionerName, ok := machine.Labels[v1alpha5.ProvisionerNameLabelKey]
	if !ok {
		return nil, fmt.Errorf("finding provisioner owner")
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: provisionerName}, provisioner); err != nil {
		return nil, fmt.Errorf("getting provisioner owner, %w", err)
	}
	instanceTypes, err := c.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return nil, fmt.Errorf("getting instance types, %w", err)
	}
	reqs := scheduling.NewNodeSelectorRequirements(machine.Spec.Requirements...)
	return lo.Filter(instanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements) == nil &&
			len(i.Offerings.Requirements(reqs).Available()) > 0 &&
			resources.Fits(machine.Spec.Resources.Requests, i.Allocatable())
	}), nil
}

func (c *CloudProvider) resolveInstanceTypeFromInstance(ctx context.Context, instance *ec2.Instance) (*cloudprovider.InstanceType, error) {
	provisioner, err := c.resolveProvisionerFromInstance(ctx, instance)
	if err != nil {
		// If we can't resolve the provisioner, we fallback to not getting instance type info
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving provisioner, %w", err))
	}
	instanceTypes, err := c.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		// If we can't resolve the provisioner, we fallback to not getting instance type info
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving node template, %w", err))
	}
	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == aws.StringValue(instance.InstanceType)
	})
	return instanceType, nil
}

func (c *CloudProvider) resolveProvisionerFromInstance(ctx context.Context, instance *ec2.Instance) (*v1alpha5.Provisioner, error) {
	provisioner := &v1alpha5.Provisioner{}
	tag, ok := lo.Find(instance.Tags, func(t *ec2.Tag) bool {
		return aws.StringValue(t.Key) == v1alpha5.ProvisionerNameLabelKey
	})
	if !ok {
		return nil, errors.NewNotFound(schema.GroupResource{Group: v1alpha5.Group, Resource: "Provisioner"}, "")
	}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: aws.StringValue(tag.Value)}, provisioner); err != nil {
		return nil, err
	}
	return provisioner, nil
}

func (c *CloudProvider) instanceToMachine(ctx context.Context, instance *ec2.Instance, instanceType *cloudprovider.InstanceType) *v1alpha5.Machine {
	machine := &v1alpha5.Machine{}
	labels := map[string]string{}

	if instanceType != nil {
		for key, req := range instanceType.Requirements {
			if req.Len() == 1 {
				labels[key] = req.Values()[0]
			}
		}
		machine.Status.Capacity = functional.FilterMap(instanceType.Capacity, func(_ v1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) })
		machine.Status.Allocatable = functional.FilterMap(instanceType.Allocatable(), func(_ v1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) })
	}
	labels[v1alpha1.LabelInstanceAMIID] = aws.StringValue(instance.ImageId)
	labels[v1.LabelTopologyZone] = aws.StringValue(instance.Placement.AvailabilityZone)
	labels[v1alpha5.LabelCapacityType] = getCapacityType(instance)
	if tag, ok := lo.Find(instance.Tags, func(t *ec2.Tag) bool { return aws.StringValue(t.Key) == v1alpha5.ProvisionerNameLabelKey }); ok {
		labels[v1alpha5.ProvisionerNameLabelKey] = aws.StringValue(tag.Value)
	}
	if tag, ok := lo.Find(instance.Tags, func(t *ec2.Tag) bool { return aws.StringValue(t.Key) == v1alpha5.ManagedByLabelKey }); ok {
		labels[v1alpha5.ManagedByLabelKey] = aws.StringValue(tag.Value)
	}
	machine.Name = lo.Ternary(
		settings.FromContext(ctx).NodeNameConvention == settings.ResourceName,
		aws.StringValue(instance.InstanceId),
		strings.ToLower(aws.StringValue(instance.PrivateDnsName)),
	)
	machine.Labels = labels
	machine.CreationTimestamp = metav1.Time{Time: aws.TimeValue(instance.LaunchTime)}
	machine.Status.ProviderID = fmt.Sprintf("aws:///%s/%s", aws.StringValue(instance.Placement.AvailabilityZone), aws.StringValue(instance.InstanceId))
	return machine
}
