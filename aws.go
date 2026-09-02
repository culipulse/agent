package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	smithy "github.com/aws/smithy-go"
)

func normalizeLoadBalancer(lb elbtypes.LoadBalancer, ports []int, tags map[string]string, region string) CloudResource {
	rt := "alb"
	if lb.Type == elbtypes.LoadBalancerTypeEnumNetwork {
		rt = "nlb"
	}
	meta := map[string]string{"region": region}
	if lb.Scheme != "" {
		meta["scheme"] = string(lb.Scheme)
	}
	if lb.State != nil {
		meta["state"] = string(lb.State.Code)
	}
	return CloudResource{
		Provider: "aws", ResourceType: rt,
		ResourceID: aws.ToString(lb.LoadBalancerArn), Address: aws.ToString(lb.DNSName),
		Ports: ports, Tags: tags, Meta: meta,
	}
}

func normalizeEC2Instance(inst ec2types.Instance, region string) CloudResource {
	addr := aws.ToString(inst.PublicIpAddress)
	public := "true"
	if addr == "" {
		addr = aws.ToString(inst.PrivateIpAddress)
		public = "false"
	}
	tags := map[string]string{}
	for _, t := range inst.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	meta := map[string]string{"region": region, "public": public}
	if inst.State != nil {
		meta["state"] = string(inst.State.Name)
	}
	if inst.VpcId != nil {
		meta["vpc"] = aws.ToString(inst.VpcId)
	}
	return CloudResource{
		Provider: "aws", ResourceType: "ec2",
		ResourceID: aws.ToString(inst.InstanceId), Address: addr,
		Ports: []int{}, Tags: tags, Meta: meta,
	}
}

func normalizeRDS(db rdstypes.DBInstance, tags map[string]string, region string) CloudResource {
	var addr string
	var ports []int
	if db.Endpoint != nil {
		addr = aws.ToString(db.Endpoint.Address)
		if db.Endpoint.Port != nil {
			ports = []int{int(aws.ToInt32(db.Endpoint.Port))}
		}
	}
	meta := map[string]string{"region": region}
	if db.Engine != nil {
		meta["engine"] = aws.ToString(db.Engine)
	}
	if db.DBInstanceStatus != nil {
		meta["status"] = aws.ToString(db.DBInstanceStatus)
	}
	return CloudResource{
		Provider: "aws", ResourceType: "rds",
		ResourceID: aws.ToString(db.DBInstanceArn), Address: addr,
		Ports: ports, Tags: tags, Meta: meta,
	}
}

// awsErr renders a short, user-actionable one-liner for a failed AWS call.
func awsErr(op, region string, err error) string {
	msg := err.Error()
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		msg = apiErr.ErrorCode() // e.g. AccessDenied, UnauthorizedOperation
	} else if len(msg) > 120 {
		msg = msg[:120]
	}
	return fmt.Sprintf("%s (%s): %s", op, region, msg)
}

// representativeError picks one short error to surface to the console.
func representativeError(errs []string) string {
	switch len(errs) {
	case 0:
		return ""
	case 1:
		return errs[0]
	default:
		return fmt.Sprintf("%s (+%d more)", errs[0], len(errs)-1)
	}
}

// collectAWS lists ALB/NLB, EC2, and RDS in one region and returns normalized resources.
func collectAWS(ctx context.Context, region string) ([]CloudResource, []string) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, []string{awsErr("aws:LoadConfig", region, err)}
	}
	var out []CloudResource
	var errs []string

	elb := elasticloadbalancingv2.NewFromConfig(cfg)
	if lbs, err := elb.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{}); err == nil {
		for _, lb := range lbs.LoadBalancers {
			var ports []int
			if ls, err := elb.DescribeListeners(ctx, &elasticloadbalancingv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}); err == nil {
				for _, l := range ls.Listeners {
					if l.Port != nil {
						ports = append(ports, int(*l.Port))
					}
				}
			}
			tags := map[string]string{}
			if td, err := elb.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{ResourceArns: []string{aws.ToString(lb.LoadBalancerArn)}}); err == nil {
				for _, d := range td.TagDescriptions {
					for _, t := range d.Tags {
						tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
					}
				}
			}
			out = append(out, normalizeLoadBalancer(lb, ports, tags, region))
		}
	} else {
		log.Printf("aws: elb describe error in %s: %v", region, err)
		errs = append(errs, awsErr("elasticloadbalancing:DescribeLoadBalancers", region, err))
	}

	e := ec2.NewFromConfig(cfg)
	if res, err := e.DescribeInstances(ctx, &ec2.DescribeInstancesInput{}); err == nil {
		for _, r := range res.Reservations {
			for _, inst := range r.Instances {
				out = append(out, normalizeEC2Instance(inst, region))
			}
		}
	} else {
		log.Printf("aws: ec2 describe error in %s: %v", region, err)
		errs = append(errs, awsErr("ec2:DescribeInstances", region, err))
	}

	r := rds.NewFromConfig(cfg)
	if dbs, err := r.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{}); err == nil {
		for _, db := range dbs.DBInstances {
			tags := map[string]string{}
			if lt, err := r.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: db.DBInstanceArn}); err == nil {
				for _, t := range lt.TagList {
					tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			}
			out = append(out, normalizeRDS(db, tags, region))
		}
	} else {
		log.Printf("aws: rds describe error in %s: %v", region, err)
		errs = append(errs, awsErr("rds:DescribeDBInstances", region, err))
	}

	return out, errs
}

// runAWSDiscovery collects all configured regions and pushes one snapshot. One pass.
func runAWSDiscovery(client *Client, regions []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var all []CloudResource
	var errs []string
	for _, region := range regions {
		rs, es := collectAWS(ctx, region)
		all = append(all, rs...)
		errs = append(errs, es...)
	}
	errMsg := representativeError(errs)
	log.Printf("aws discovery: %d resources across %d region(s), %d error(s)", len(all), len(regions), len(errs))
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pcancel()
	if err := client.CloudResources(pctx, "aws", all, errMsg); err != nil {
		log.Printf("aws discovery report error: %v", err)
	}
}

// parseRegions splits a comma list; empty falls back to [fallback] (the agent's own region).
func parseRegions(s, fallback string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 && fallback != "" {
		out = []string{fallback}
	}
	return out
}
