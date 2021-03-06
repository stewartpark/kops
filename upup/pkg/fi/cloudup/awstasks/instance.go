/*
Copyright 2016 The Kubernetes Authors.

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

package awstasks

import (
	"fmt"

	"encoding/base64"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"strings"
)

const MaxUserDataSize = 16384

type Instance struct {
	ID *string

	UserData fi.Resource

	Subnet           *Subnet
	PrivateIPAddress *string

	Name *string
	Tags map[string]string

	ImageID            *string
	InstanceType       *string
	SSHKey             *SSHKey
	SecurityGroups     []*SecurityGroup
	AssociatePublicIP  *bool
	IAMInstanceProfile *IAMInstanceProfile
}

var _ fi.CompareWithID = &Instance{}

func (s *Instance) CompareWithID() *string {
	return s.ID
}

func (e *Instance) Find(c *fi.Context) (*Instance, error) {
	cloud := c.Cloud.(awsup.AWSCloud)

	filters := cloud.BuildFilters(e.Name)
	filters = append(filters, awsup.NewEC2Filter("instance-state-name", "pending", "running", "stopping", "stopped"))
	request := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	response, err := cloud.EC2().DescribeInstances(request)
	if err != nil {
		return nil, fmt.Errorf("error listing instances: %v", err)
	}

	instances := []*ec2.Instance{}
	if response != nil {
		for _, reservation := range response.Reservations {
			for _, instance := range reservation.Instances {
				instances = append(instances, instance)
			}
		}
	}

	if len(instances) == 0 {
		return nil, nil
	}

	if len(instances) != 1 {
		return nil, fmt.Errorf("found multiple Instances with name: %s", *e.Name)
	}

	glog.V(2).Info("found existing instance")
	i := instances[0]

	if i.InstanceId == nil {
		return nil, fmt.Errorf("found instance, but InstanceId was nil")
	}

	actual := &Instance{
		ID:               i.InstanceId,
		PrivateIPAddress: i.PrivateIpAddress,
		InstanceType:     i.InstanceType,
		ImageID:          i.ImageId,
		Name:             findNameTag(i.Tags),
	}

	// Fetch instance UserData
	{
		request := &ec2.DescribeInstanceAttributeInput{}
		request.InstanceId = i.InstanceId
		request.Attribute = aws.String("userData")
		response, err := cloud.EC2().DescribeInstanceAttribute(request)
		if err != nil {
			return nil, fmt.Errorf("error querying EC2 for user metadata for instance %q: %v", *i.InstanceId, err)
		}
		if response.UserData != nil {
			b, err := base64.StdEncoding.DecodeString(aws.StringValue(response.UserData.Value))
			if err != nil {
				return nil, fmt.Errorf("error decoding EC2 UserData: %v", err)
			}
			actual.UserData = fi.NewBytesResource(b)
		}
	}

	if i.SubnetId != nil {
		actual.Subnet = &Subnet{ID: i.SubnetId}
	}
	if i.KeyName != nil {
		actual.SSHKey = &SSHKey{Name: i.KeyName}
	}

	for _, sg := range i.SecurityGroups {
		actual.SecurityGroups = append(actual.SecurityGroups, &SecurityGroup{ID: sg.GroupId})
	}

	associatePublicIpAddress := false
	for _, ni := range i.NetworkInterfaces {
		if aws.StringValue(ni.Association.PublicIp) != "" {
			associatePublicIpAddress = true
		}
	}
	actual.AssociatePublicIP = &associatePublicIpAddress

	if i.IamInstanceProfile != nil {
		actual.IAMInstanceProfile = &IAMInstanceProfile{Name: nameFromIAMARN(i.IamInstanceProfile.Arn)}
	}

	actual.Tags = mapEC2TagsToMap(i.Tags)

	e.ID = actual.ID

	// Avoid spurious changes on ImageId
	if e.ImageID != nil && actual.ImageID != nil && *actual.ImageID != *e.ImageID {
		image, err := cloud.ResolveImage(*e.ImageID)
		if err != nil {
			glog.Warningf("unable to resolve image: %q: %v", *e.ImageID, err)
		} else if image == nil {
			glog.Warningf("unable to resolve image: %q: not found", *e.ImageID)
		} else if aws.StringValue(image.ImageId) == *actual.ImageID {
			glog.V(4).Infof("Returning matching ImageId as expected name: %q -> %q", *actual.ImageID, *e.ImageID)
			actual.ImageID = e.ImageID
		}
	}

	return actual, nil
}

func nameFromIAMARN(arn *string) *string {
	if arn == nil {
		return nil
	}
	tokens := strings.Split(*arn, ":")
	last := tokens[len(tokens)-1]

	if !strings.HasPrefix(last, "instance-profile/") {
		glog.Warningf("Unexpected ARN for instance profile: %q", *arn)
	}

	name := strings.TrimPrefix(last, "instance-profile/")
	return &name
}

func (e *Instance) Run(c *fi.Context) error {
	cloud := c.Cloud.(awsup.AWSCloud)

	cloud.AddTags(e.Name, e.Tags)

	return fi.DefaultDeltaRunMethod(e, c)
}

func (_ *Instance) CheckChanges(a, e, changes *Instance) error {
	if a != nil {
		if e.Name == nil {
			return fi.RequiredField("Name")
		}
	}
	return nil
}

func (_ *Instance) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *Instance) error {
	if a == nil {
		if e.ImageID == nil {
			return fi.RequiredField("ImageID")
		}
		image, err := t.Cloud.ResolveImage(*e.ImageID)
		if err != nil {
			return err
		}

		glog.V(2).Infof("Creating Instance with Name:%q", *e.Name)
		request := &ec2.RunInstancesInput{
			ImageId:      image.ImageId,
			InstanceType: e.InstanceType,
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
		}

		if e.SSHKey != nil {
			request.KeyName = e.SSHKey.Name
		}

		securityGroupIDs := []*string{}
		for _, sg := range e.SecurityGroups {
			securityGroupIDs = append(securityGroupIDs, sg.ID)
		}
		request.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
			{
				DeviceIndex:              aws.Int64(0),
				AssociatePublicIpAddress: e.AssociatePublicIP,
				SubnetId:                 e.Subnet.ID,
				PrivateIpAddress:         e.PrivateIPAddress,
				Groups:                   securityGroupIDs,
			},
		}

		// Build up the actual block device mappings
		// TODO: Support RootVolumeType & RootVolumeSize (see launchconfiguration)
		blockDeviceMappings, err := buildEphemeralDevices(e.InstanceType)
		if err != nil {
			return err
		}

		if len(blockDeviceMappings) != 0 {
			request.BlockDeviceMappings = []*ec2.BlockDeviceMapping{}
			for deviceName, bdm := range blockDeviceMappings {
				request.BlockDeviceMappings = append(request.BlockDeviceMappings, bdm.ToEC2(deviceName))
			}
		}

		if e.UserData != nil {
			d, err := fi.ResourceAsBytes(e.UserData)
			if err != nil {
				return fmt.Errorf("error rendering Instance UserData: %v", err)
			}
			if len(d) > MaxUserDataSize {
				// TODO: Re-enable gzip?
				// But it exposes some bugs in the AWS console, so if we can avoid it, we should
				//d, err = fi.GzipBytes(d)
				//if err != nil {
				//	return fmt.Errorf("error while gzipping UserData: %v", err)
				//}
				return fmt.Errorf("Instance UserData was too large (%d bytes)", len(d))
			}
			request.UserData = aws.String(base64.StdEncoding.EncodeToString(d))
		}

		if e.IAMInstanceProfile != nil {
			request.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{
				Name: e.IAMInstanceProfile.Name,
			}
		}

		response, err := t.Cloud.EC2().RunInstances(request)
		if err != nil {
			return fmt.Errorf("error creating Instance: %v", err)
		}

		e.ID = response.Instances[0].InstanceId
	}

	return t.AddAWSTags(*e.ID, e.Tags)
}

func (e *Instance) TerraformLink() *terraform.Literal {
	return terraform.LiteralSelfLink("aws_instance", *e.Name)
}
