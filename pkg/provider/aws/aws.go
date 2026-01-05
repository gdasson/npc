package aws

import (
    "context"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/autoscaling"
    "github.com/aws/aws-sdk-go-v2/service/ec2"
    ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
    "github.com/myorg/nodepatcher/pkg/provider"
)

const (
    TagClusterID = "nodepatcher.io/cluster-id"
    TagRunUID    = "nodepatcher.io/run-uid"
    TagTaskName  = "nodepatcher.io/task"
    TagState     = "nodepatcher.io/state"
    TagReplaces  = "nodepatcher.io/replaces"
    TagCreatedAt = "nodepatcher.io/created-at"
)

type Provider struct {
    ec2    *ec2.Client
    asg    *autoscaling.Client
    region string
}

func New(cfg aws.Config, region string) *Provider {
    return &Provider{
        ec2:    ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.Region = region }),
        asg:    autoscaling.NewFromConfig(cfg, func(o *autoscaling.Options) { o.Region = region }),
        region: region,
    }
}

func (p *Provider) Resolve(ctx context.Context, nodeProviderID string) (provider.MachineRef, any, error) {
    instID := extractInstanceID(nodeProviderID)
    if instID == "" {
        return provider.MachineRef{}, nil, fmt.Errorf("cannot parse aws instance id from providerID=%q", nodeProviderID)
    }

    di, err := p.asg.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{
        InstanceIds: []string{instID},
    })
    if err != nil || len(di.AutoScalingInstances) == 0 {
        return provider.MachineRef{}, nil, fmt.Errorf("DescribeAutoScalingInstances: %w", err)
    }
    asgName := aws.ToString(di.AutoScalingInstances[0].AutoScalingGroupName)

    ec2Out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instID}})
    if err != nil {
        return provider.MachineRef{}, nil, err
    }
    inst := ec2Out.Reservations[0].Instances[0]
    subnet := aws.ToString(inst.SubnetId)

    ltID, ltVer := "", ""
    if inst.LaunchTemplate != nil {
        ltID = aws.ToString(inst.LaunchTemplate.LaunchTemplateId)
        ltVer = aws.ToString(inst.LaunchTemplate.Version)
    }

    ref := provider.MachineRef{Provider: "aws", ID: instID, Location: map[string]string{}}
    details := &v1alpha1.AWSDetails{
        Region:                p.region,
        ASGName:               asgName,
        SubnetID:              subnet,
        LaunchTemplateID:      ltID,
        LaunchTemplateVersion: ltVer,
    }
    return ref, details, nil
}

func (p *Provider) EnsureReplacement(ctx context.Context, old provider.MachineRef, oldDetailsAny any, spec provider.ReplacementSpec) (provider.MachineRef, any, error) {
    d, ok := oldDetailsAny.(*v1alpha1.AWSDetails)
    if !ok || d == nil || d.ASGName == "" || d.SubnetID == "" {
        return provider.MachineRef{}, nil, errors.New("aws EnsureReplacement requires AWSDetails with ASGName and SubnetID")
    }
    // idempotency: reuse if already created for this run+old
    existing, err := p.findInstanceByTags(ctx, spec.ClusterID, spec.RunUID, old.ID)
    if err == nil && existing != "" {
        ref := provider.MachineRef{Provider: "aws", ID: existing}
        nd := *d
        return ref, &nd, nil
    }

    tags := map[string]string{
        TagClusterID: spec.ClusterID,
        TagRunUID:    spec.RunUID,
        TagTaskName:  spec.TaskName,
        TagState:     "created",
        TagReplaces:  old.ID,
        TagCreatedAt: time.Now().UTC().Format(time.RFC3339),
    }
    for k, v := range spec.ExtraTags {
        tags[k] = v
    }

    subnetID := d.SubnetID

    runIn := &ec2.RunInstancesInput{
        MinCount: aws.Int32(1),
        MaxCount: aws.Int32(1),
        SubnetId: aws.String(subnetID),
        TagSpecifications: []ec2types.TagSpecification{
            {ResourceType: ec2types.ResourceTypeInstance, Tags: toEC2Tags(tags)},
        },
    }
    if d.LaunchTemplateID != "" {
        runIn.LaunchTemplate = &ec2types.LaunchTemplateSpecification{
            LaunchTemplateId: aws.String(d.LaunchTemplateID),
            Version:          aws.String(defaultLTVersion(d.LaunchTemplateVersion)),
        }
    }

    ri, err := p.ec2.RunInstances(ctx, runIn)
    if err != nil || len(ri.Instances) == 0 {
        return provider.MachineRef{}, nil, fmt.Errorf("RunInstances: %w", err)
    }
    newID := aws.ToString(ri.Instances[0].InstanceId)

    // Attach to SAME ASG
    _, err = p.asg.AttachInstances(ctx, &autoscaling.AttachInstancesInput{
        AutoScalingGroupName: aws.String(d.ASGName),
        InstanceIds:          []string{newID},
    })
    if err != nil {
        return provider.MachineRef{}, nil, fmt.Errorf("AttachInstances: %w", err)
    }

    ref := provider.MachineRef{Provider: "aws", ID: newID}
    nd := *d
    return ref, &nd, nil
}

func (p *Provider) NodeMatchesMachine(nodeProviderID string, machine provider.MachineRef) (bool, error) {
    if machine.Provider != "aws" {
        return false, fmt.Errorf("aws provider cannot match non-aws machine")
    }
    return extractInstanceID(nodeProviderID) == machine.ID, nil
}

func (p *Provider) MarkState(ctx context.Context, machine provider.MachineRef, state string, tags map[string]string) error {
    if machine.Provider != "aws" || machine.ID == "" {
        return nil
    }
    all := map[string]string{TagState: state}
    for k, v := range tags {
        all[k] = v
    }
    _, err := p.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
        Resources: []string{machine.ID},
        Tags:      toEC2Tags(all),
    })
    return err
}

func (p *Provider) SetProtection(ctx context.Context, machine provider.MachineRef, detailsAny any, protected bool) error {
    d, ok := detailsAny.(*v1alpha1.AWSDetails)
    if !ok || d == nil || d.ASGName == "" || machine.ID == "" {
        return nil
    }
    _, err := p.asg.SetInstanceProtection(ctx, &autoscaling.SetInstanceProtectionInput{
        AutoScalingGroupName: aws.String(d.ASGName),
        InstanceIds:          []string{machine.ID},
        ProtectedFromScaleIn: aws.Bool(protected),
    })
    return err
}

func (p *Provider) DeleteMachine(ctx context.Context, machine provider.MachineRef, detailsAny any) error {
    d, ok := detailsAny.(*v1alpha1.AWSDetails)
    if !ok || d == nil || d.ASGName == "" {
        return errors.New("aws DeleteMachine requires AWSDetails with ASGName")
    }

    // Detach (decrement desired) then terminate
    _, _ = p.asg.DetachInstances(ctx, &autoscaling.DetachInstancesInput{
        AutoScalingGroupName:           aws.String(d.ASGName),
        InstanceIds:                    []string{machine.ID},
        ShouldDecrementDesiredCapacity: aws.Bool(true),
    })

    _, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
        InstanceIds: []string{machine.ID},
    })
    return err
}

func (p *Provider) CleanupRunOrphans(ctx context.Context, clusterID, runUID string, hasNode func(instanceID string) (bool, error)) error {
    filters := []ec2types.Filter{
        {Name: aws.String("tag:" + TagClusterID), Values: []string{clusterID}},
        {Name: aws.String("tag:" + TagRunUID), Values: []string{runUID}},
    }
    out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
    if err != nil {
        return err
    }
    for _, r := range out.Reservations {
        for _, inst := range r.Instances {
            id := aws.ToString(inst.InstanceId)
            st := tagValue(inst.Tags, TagState)
            if st == "completed" {
                continue
            }
            joined, err := hasNode(id)
            if err != nil {
                continue
            }
            if joined {
                continue
            }
            // never joined -> safe to terminate
            _, _ = p.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
                Resources: []string{id},
                Tags:      toEC2Tags(map[string]string{TagState: "orphan-candidate"}),
            })
            _, _ = p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
        }
    }
    return nil
}

func (p *Provider) findInstanceByTags(ctx context.Context, clusterID, runUID, replaces string) (string, error) {
    filters := []ec2types.Filter{
        {Name: aws.String("tag:" + TagClusterID), Values: []string{clusterID}},
        {Name: aws.String("tag:" + TagRunUID), Values: []string{runUID}},
        {Name: aws.String("tag:" + TagReplaces), Values: []string{replaces}},
    }
    out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
    if err != nil {
        return "", err
    }
    for _, r := range out.Reservations {
        for _, inst := range r.Instances {
            if inst.State != nil && inst.State.Name == ec2types.InstanceStateNameTerminated {
                continue
            }
            return aws.ToString(inst.InstanceId), nil
        }
    }
    return "", errors.New("not found")
}

func extractInstanceID(providerID string) string {
    if providerID == "" {
        return ""
    }
    parts := strings.Split(providerID, "/")
    last := parts[len(parts)-1]
    if strings.HasPrefix(last, "i-") {
        return last
    }
    if strings.HasPrefix(providerID, "i-") {
        return providerID
    }
    return ""
}

func defaultLTVersion(v string) string {
    if v == "" {
        return "$Latest"
    }
    return v
}

func toEC2Tags(m map[string]string) []ec2types.Tag {
    out := make([]ec2types.Tag, 0, len(m))
    for k, v := range m {
        out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
    }
    return out
}

func tagValue(tags []ec2types.Tag, key string) string {
    for _, t := range tags {
        if aws.ToString(t.Key) == key {
            return aws.ToString(t.Value)
        }
    }
    return ""
}
