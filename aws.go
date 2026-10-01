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
	} else if errors.Is(err, context.DeadlineExceeded) {
		msg = "timed out" // the SDK's wrapper text is long; truncating it would cut off the cause
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

// elbAPI is the slice of the ELBv2 client discovery uses — an interface so tests can serve
// multi-page results and sub-call failures.
type elbAPI interface {
	elasticloadbalancingv2.DescribeLoadBalancersAPIClient
	elasticloadbalancingv2.DescribeListenersAPIClient
	DescribeTags(context.Context, *elasticloadbalancingv2.DescribeTagsInput, ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTagsOutput, error)
}

// awsClients are the per-region AWS clients one collectRegion pass reads from.
type awsClients struct {
	elb elbAPI
	ec2 ec2.DescribeInstancesAPIClient
	rds rds.DescribeDBInstancesAPIClient
}

// elbTagsBatch is DescribeTags' per-call ARN limit.
const elbTagsBatch = 20

// collectAWS lists ALB/NLB, EC2, and RDS in one region and returns normalized resources.
func collectAWS(ctx context.Context, region string) ([]CloudResource, []string) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, []string{awsErr("aws:LoadConfig", region, err)}
	}
	return collectRegion(ctx, region, awsClients{
		elb: elasticloadbalancingv2.NewFromConfig(cfg),
		ec2: ec2.NewFromConfig(cfg),
		rds: rds.NewFromConfig(cfg),
	})
}

// collectRegion reads every page of every service. A failed page or sub-call is reported in errs —
// the server then closes nothing for this scan — so a partial scan never looks clean.
func collectRegion(ctx context.Context, region string, c awsClients) ([]CloudResource, []string) {
	var out []CloudResource
	var errs []string
	for _, collect := range []func() ([]CloudResource, []string){
		func() ([]CloudResource, []string) { return collectLoadBalancers(ctx, region, c.elb) },
		func() ([]CloudResource, []string) { return collectEC2(ctx, region, c.ec2) },
		func() ([]CloudResource, []string) { return collectRDS(ctx, region, c.rds) },
	} {
		rs, es := collect()
		out = append(out, rs...)
		errs = append(errs, es...)
	}
	return out, errs
}

// collectLoadBalancers lists every load balancer, then its tags (batched) and listener ports.
// A load balancer whose tags or listeners can't be read is left out rather than sent with empty
// ports/tags: the server would overwrite the stored values with the empty ones, and could flag its
// monitor orphaned. Left out, it keeps its last good data (an errored scan closes nothing).
func collectLoadBalancers(ctx context.Context, region string, api elbAPI) ([]CloudResource, []string) {
	var errs []string
	var lbs []elbtypes.LoadBalancer
	p := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(api, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeLoadBalancers", region, err))
			break
		}
		lbs = append(lbs, pg.LoadBalancers...)
	}

	tags := map[string]map[string]string{}
	for start := 0; start < len(lbs); start += elbTagsBatch {
		arns := make([]string, 0, elbTagsBatch)
		for _, lb := range lbs[start:min(start+elbTagsBatch, len(lbs))] {
			arns = append(arns, aws.ToString(lb.LoadBalancerArn))
		}
		td, err := api.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{ResourceArns: arns})
		if err != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeTags", region, err))
			continue
		}
		for _, arn := range arns {
			tags[arn] = map[string]string{} // untagged LBs still get a (empty) map
		}
		for _, d := range td.TagDescriptions {
			m := tags[aws.ToString(d.ResourceArn)]
			if m == nil {
				continue
			}
			for _, t := range d.Tags {
				m[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
	}

	var out []CloudResource
	for _, lb := range lbs {
		t, ok := tags[aws.ToString(lb.LoadBalancerArn)]
		if !ok {
			continue // its tag batch failed (already reported)
		}
		ports, err := listenerPorts(ctx, api, lb.LoadBalancerArn)
		if err != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeListeners", region, err))
			continue
		}
		out = append(out, normalizeLoadBalancer(lb, ports, t, region))
	}
	return out, errs
}

// listenerPorts returns every listener port of one load balancer, across all pages.
func listenerPorts(ctx context.Context, api elasticloadbalancingv2.DescribeListenersAPIClient, arn *string) ([]int, error) {
	var ports []int
	p := elasticloadbalancingv2.NewDescribeListenersPaginator(api, &elasticloadbalancingv2.DescribeListenersInput{LoadBalancerArn: arn})
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, l := range pg.Listeners {
			if l.Port != nil {
				ports = append(ports, int(*l.Port))
			}
		}
	}
	return ports, nil
}

// collectEC2 lists every instance across all pages. A failed page stops the listing and keeps
// what earlier pages returned.
func collectEC2(ctx context.Context, region string, api ec2.DescribeInstancesAPIClient) ([]CloudResource, []string) {
	var out []CloudResource
	p := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{})
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			return out, []string{awsErr("ec2:DescribeInstances", region, err)}
		}
		for _, r := range pg.Reservations {
			for _, inst := range r.Instances {
				out = append(out, normalizeEC2Instance(inst, region))
			}
		}
	}
	return out, nil
}

// collectRDS lists every DB instance across all pages. Tags come back inline in TagList, so there
// is no per-instance tag call.
func collectRDS(ctx context.Context, region string, api rds.DescribeDBInstancesAPIClient) ([]CloudResource, []string) {
	var out []CloudResource
	p := rds.NewDescribeDBInstancesPaginator(api, &rds.DescribeDBInstancesInput{})
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			return out, []string{awsErr("rds:DescribeDBInstances", region, err)}
		}
		for _, db := range pg.DBInstances {
			tags := map[string]string{}
			for _, t := range db.TagList {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			out = append(out, normalizeRDS(db, tags, region))
		}
	}
	return out, nil
}

// awsRegionTimeout bounds ONE region's scan. Each region gets its own, so a slow or throttled
// region can't use up the time of the regions after it.
var awsRegionTimeout = 60 * time.Second

// collectAllRegions scans regions in order, each under its own deadline.
func collectAllRegions(regions []string, perRegion time.Duration, collect func(context.Context, string) ([]CloudResource, []string)) ([]CloudResource, []string) {
	var all []CloudResource
	var errs []string
	for _, region := range regions {
		ctx, cancel := context.WithTimeout(context.Background(), perRegion)
		rs, es := collect(ctx, region)
		cancel()
		all = append(all, rs...)
		errs = append(errs, es...)
	}
	return all, errs
}

// runAWSDiscovery collects all configured regions and pushes one snapshot. One pass.
func runAWSDiscovery(client *Client, regions []string) {
	all, errs := collectAllRegions(regions, awsRegionTimeout, collectAWS)
	for _, e := range errs {
		log.Printf("aws discovery: %s", e)
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
