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

// errRepeatedToken is what a paginator that stopped on a repeated next-page token is reported as. The SDK
// stops such a loop silently (StopOnDuplicateToken), which would otherwise look like a complete listing.
var errRepeatedToken = errors.New("repeated page token")

// ScannedPair is one (resource type, region) combination whose listing completed with no error. The
// server closes a vanished resource only when its pair is scanned, so a persistent error in one
// service or region doesn't stop pruning everywhere else.
type ScannedPair struct {
	ResourceType string `json:"resourceType"`
	Region       string `json:"region"`
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
func collectAWS(ctx context.Context, region string) ([]CloudResource, []string, []ScannedPair) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, []string{awsErr("aws:LoadConfig", region, err)}, nil
	}
	return collectRegion(ctx, region, awsClients{
		elb: elasticloadbalancingv2.NewFromConfig(cfg),
		ec2: ec2.NewFromConfig(cfg),
		rds: rds.NewFromConfig(cfg),
	})
}

// collectRegion reads every page of every service. A failed page or sub-call is reported in errs, so a
// partial scan never looks clean. scanned lists the resource types whose listing finished with no error.
func collectRegion(ctx context.Context, region string, c awsClients) ([]CloudResource, []string, []ScannedPair) {
	var out []CloudResource
	var errs []string
	var scanned []ScannedPair
	for _, svc := range []struct {
		types   []string
		collect func() ([]CloudResource, []string)
	}{
		{[]string{"alb", "nlb"}, func() ([]CloudResource, []string) { return collectLoadBalancers(ctx, region, c.elb) }},
		{[]string{"ec2"}, func() ([]CloudResource, []string) { return collectEC2(ctx, region, c.ec2) }},
		{[]string{"rds"}, func() ([]CloudResource, []string) { return collectRDS(ctx, region, c.rds) }},
	} {
		rs, es := svc.collect()
		out = append(out, rs...)
		errs = append(errs, es...)
		if len(es) == 0 {
			for _, rt := range svc.types {
				scanned = append(scanned, ScannedPair{ResourceType: rt, Region: region})
			}
		}
	}
	return out, errs, scanned
}

// collectLoadBalancers lists every load balancer, then its tags (batched) and listener ports.
// A load balancer whose tags or listeners can't be read is left out rather than sent with empty
// ports/tags: the server would overwrite the stored values with the empty ones, and could flag its
// monitor orphaned. Left out, it keeps its last good data (an errored scan closes nothing).
func collectLoadBalancers(ctx context.Context, region string, api elbAPI) ([]CloudResource, []string) {
	var errs []string
	var lbs []elbtypes.LoadBalancer
	p := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(api, &elasticloadbalancingv2.DescribeLoadBalancersInput{},
		func(o *elasticloadbalancingv2.DescribeLoadBalancersPaginatorOptions) { o.StopOnDuplicateToken = true })
	var lastMarker *string
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeLoadBalancers", region, err))
			lastMarker = nil
			break
		}
		lbs = append(lbs, pg.LoadBalancers...)
		lastMarker = pg.NextMarker
	}
	if aws.ToString(lastMarker) != "" { // stopped on a repeated token with more pages claimed (an empty token is the SDK's end marker)
		errs = append(errs, awsErr("elasticloadbalancing:DescribeLoadBalancers", region, errRepeatedToken))
	}

	tags := map[string]map[string]string{}
	for start := 0; start < len(lbs); start += elbTagsBatch {
		arns := make([]string, 0, elbTagsBatch)
		for _, lb := range lbs[start:min(start+elbTagsBatch, len(lbs))] {
			arns = append(arns, aws.ToString(lb.LoadBalancerArn))
		}
		td, err := api.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{ResourceArns: arns})
		if err == nil {
			addTags(tags, arns, td)
			continue
		}
		// One bad ARN (a resource-scoped deny, or an LB deleted since it was listed) fails the whole batch.
		// Retry one ARN at a time so only the LB(s) that really can't be read are left out.
		if len(arns) == 1 || ctx.Err() != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeTags", region, err))
			continue
		}
		failed := 0
		var lastErr error
		for _, arn := range arns {
			if ctx.Err() != nil {
				failed, lastErr = failed+1, ctx.Err()
				continue
			}
			one, oerr := api.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{ResourceArns: []string{arn}})
			if oerr != nil {
				failed, lastErr = failed+1, oerr
				continue
			}
			addTags(tags, []string{arn}, one)
		}
		if failed > 0 {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeTags", region, lastErr))
		}
	}

	var out []CloudResource
	for _, lb := range lbs {
		t, ok := tags[aws.ToString(lb.LoadBalancerArn)]
		if !ok {
			continue // its tags couldn't be read (already reported)
		}
		// Once the region's deadline passes every remaining call would fail the same way: report it once
		// and stop, instead of one error per load balancer (and no time left for EC2/RDS).
		if ctx.Err() != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeListeners", region, ctx.Err()))
			break
		}
		ports, err := listenerPorts(ctx, api, lb.LoadBalancerArn)
		if err != nil {
			errs = append(errs, awsErr("elasticloadbalancing:DescribeListeners", region, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		out = append(out, normalizeLoadBalancer(lb, ports, t, region))
	}
	return out, errs
}

// addTags records the tags DescribeTags returned for arns; an LB with no tags still gets an empty map.
func addTags(tags map[string]map[string]string, arns []string, td *elasticloadbalancingv2.DescribeTagsOutput) {
	for _, arn := range arns {
		tags[arn] = map[string]string{}
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

// listenerPorts returns every listener port of one load balancer, across all pages.
func listenerPorts(ctx context.Context, api elasticloadbalancingv2.DescribeListenersAPIClient, arn *string) ([]int, error) {
	var ports []int
	p := elasticloadbalancingv2.NewDescribeListenersPaginator(api, &elasticloadbalancingv2.DescribeListenersInput{LoadBalancerArn: arn},
		func(o *elasticloadbalancingv2.DescribeListenersPaginatorOptions) { o.StopOnDuplicateToken = true })
	var lastMarker *string
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
		lastMarker = pg.NextMarker
	}
	if aws.ToString(lastMarker) != "" {
		return nil, errRepeatedToken
	}
	return ports, nil
}

// collectEC2 lists every instance across all pages. A failed page stops the listing and keeps
// what earlier pages returned.
func collectEC2(ctx context.Context, region string, api ec2.DescribeInstancesAPIClient) ([]CloudResource, []string) {
	var out []CloudResource
	p := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{},
		func(o *ec2.DescribeInstancesPaginatorOptions) { o.StopOnDuplicateToken = true })
	var lastToken *string
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
		lastToken = pg.NextToken
	}
	if aws.ToString(lastToken) != "" {
		return out, []string{awsErr("ec2:DescribeInstances", region, errRepeatedToken)}
	}
	return out, nil
}

// collectRDS lists every DB instance across all pages. Tags come back inline in TagList, so there
// is no per-instance tag call.
func collectRDS(ctx context.Context, region string, api rds.DescribeDBInstancesAPIClient) ([]CloudResource, []string) {
	var out []CloudResource
	p := rds.NewDescribeDBInstancesPaginator(api, &rds.DescribeDBInstancesInput{},
		func(o *rds.DescribeDBInstancesPaginatorOptions) { o.StopOnDuplicateToken = true })
	var lastMarker *string
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			return out, []string{awsErr("rds:DescribeDBInstances", region, err)}
		}
		lastMarker = pg.Marker
		for _, db := range pg.DBInstances {
			tags := map[string]string{}
			for _, t := range db.TagList {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			out = append(out, normalizeRDS(db, tags, region))
		}
	}
	if aws.ToString(lastMarker) != "" {
		return out, []string{awsErr("rds:DescribeDBInstances", region, errRepeatedToken)}
	}
	return out, nil
}

// awsRegionTimeout bounds ONE region's scan. Each region gets its own, so a slow or throttled
// region can't use up the time of the regions after it.
var awsRegionTimeout = 60 * time.Second

// collectAllRegions scans regions in order, each under its own deadline.
func collectAllRegions(regions []string, perRegion time.Duration, collect func(context.Context, string) ([]CloudResource, []string, []ScannedPair)) ([]CloudResource, []string, []ScannedPair) {
	var all []CloudResource
	var errs []string
	scanned := []ScannedPair{} // never nil: a present (even empty) list tells the server this agent reports per-pair results
	for _, region := range regions {
		ctx, cancel := context.WithTimeout(context.Background(), perRegion)
		rs, es, ps := collect(ctx, region)
		cancel()
		all = append(all, rs...)
		errs = append(errs, es...)
		scanned = append(scanned, ps...)
	}
	return all, errs, scanned
}

// runAWSDiscovery collects all configured regions and pushes one snapshot. One pass.
func runAWSDiscovery(client *Client, regions []string) {
	all, errs, scanned := collectAllRegions(regions, awsRegionTimeout, collectAWS)
	for _, e := range errs {
		log.Printf("aws discovery: %s", e)
	}
	errMsg := representativeError(errs)
	log.Printf("aws discovery: %d resources across %d region(s), %d error(s)", len(all), len(regions), len(errs))
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pcancel()
	if err := client.CloudResources(pctx, "aws", all, errMsg, scanned); err != nil {
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
