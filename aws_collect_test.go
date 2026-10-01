package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
)

// page serves fake paginated results: tok "" / nil is page 0; the returned next token is the next
// page's index, nil on the last page.
func page[T any](pages [][]T, tok *string) ([]T, *string, int) {
	if len(pages) == 0 {
		return nil, nil, 0
	}
	i := 0
	if tok != nil {
		i, _ = strconv.Atoi(*tok)
	}
	var next *string
	if i+1 < len(pages) {
		next = aws.String(strconv.Itoa(i + 1))
	}
	return pages[i], next, i
}

// hasErr reports whether any errs entry contains want.
func hasErr(errs []string, want string) bool {
	for _, e := range errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

func lbN(i int) elbtypes.LoadBalancer {
	return elbtypes.LoadBalancer{
		LoadBalancerArn: aws.String(fmt.Sprintf("arn:lb:%d", i)),
		DNSName:         aws.String(fmt.Sprintf("lb%d.elb", i)),
		Type:            elbtypes.LoadBalancerTypeEnumApplication,
	}
}

type fakeELB struct {
	pages          [][]elbtypes.LoadBalancer
	failPage       map[int]bool                 // DescribeLoadBalancers page index → throttled
	ports          map[string][]int32           // by LB ARN
	listenerErr    map[string]error             // by LB ARN
	blockListeners bool                         // DescribeListeners waits for ctx to end
	tags           map[string]map[string]string // by LB ARN
	tagErrOnCall   int                          // 1-based DescribeTags call that fails; 0 = none
	tagCalls       [][]string
}

func (f *fakeELB) DescribeLoadBalancers(ctx context.Context, in *elbv2.DescribeLoadBalancersInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeLoadBalancersOutput, error) {
	items, next, i := page(f.pages, in.Marker)
	if f.failPage[i] {
		return nil, errors.New("Throttling: Rate exceeded")
	}
	return &elbv2.DescribeLoadBalancersOutput{LoadBalancers: items, NextMarker: next}, nil
}

func (f *fakeELB) DescribeListeners(ctx context.Context, in *elbv2.DescribeListenersInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeListenersOutput, error) {
	if f.blockListeners {
		<-ctx.Done()
		return nil, fmt.Errorf("operation error Elastic Load Balancing v2: DescribeListeners, https response error: %w", ctx.Err())
	}
	arn := aws.ToString(in.LoadBalancerArn)
	if err := f.listenerErr[arn]; err != nil {
		return nil, err
	}
	var ls []elbtypes.Listener
	for _, p := range f.ports[arn] {
		ls = append(ls, elbtypes.Listener{Port: aws.Int32(p)})
	}
	return &elbv2.DescribeListenersOutput{Listeners: ls}, nil
}

func (f *fakeELB) DescribeTags(ctx context.Context, in *elbv2.DescribeTagsInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeTagsOutput, error) {
	f.tagCalls = append(f.tagCalls, in.ResourceArns)
	if len(in.ResourceArns) > 20 { // the real API rejects more than 20
		return nil, errors.New("ValidationError: too many resource ARNs")
	}
	if f.tagErrOnCall == len(f.tagCalls) {
		return nil, errors.New("AccessDenied")
	}
	out := &elbv2.DescribeTagsOutput{}
	for _, a := range in.ResourceArns {
		var ts []elbtypes.Tag
		for k, v := range f.tags[a] {
			ts = append(ts, elbtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		out.TagDescriptions = append(out.TagDescriptions, elbtypes.TagDescription{ResourceArn: aws.String(a), Tags: ts})
	}
	return out, nil
}

type fakeEC2 struct {
	pages    [][]ec2types.Instance
	failPage map[int]bool
}

func (f *fakeEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	items, next, i := page(f.pages, in.NextToken)
	if f.failPage[i] {
		return nil, errors.New("RequestLimitExceeded")
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: items}}, NextToken: next}, nil
}

type fakeRDS struct {
	pages    [][]rdstypes.DBInstance
	failPage map[int]bool
}

func (f *fakeRDS) DescribeDBInstances(ctx context.Context, in *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	items, next, i := page(f.pages, in.Marker)
	if f.failPage[i] {
		return nil, errors.New("Throttling")
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: items, Marker: next}, nil
}

// elbFixture: n LBs split into pages of `per`, each with listener 443 and tag Name=lbN, except lb 0
// which has no tags at all.
func elbFixture(n, per int) *fakeELB {
	f := &fakeELB{ports: map[string][]int32{}, tags: map[string]map[string]string{}}
	var cur []elbtypes.LoadBalancer
	for i := 0; i < n; i++ {
		lb := lbN(i)
		arn := aws.ToString(lb.LoadBalancerArn)
		f.ports[arn] = []int32{443}
		if i > 0 {
			f.tags[arn] = map[string]string{"Name": fmt.Sprintf("lb%d", i)}
		}
		cur = append(cur, lb)
		if len(cur) == per {
			f.pages, cur = append(f.pages, cur), nil
		}
	}
	if len(cur) > 0 {
		f.pages = append(f.pages, cur)
	}
	return f
}

func TestCollectLoadBalancersReadsAllPagesAndBatchesTags(t *testing.T) {
	f := elbFixture(45, 20) // pages of 20, 20, 5
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if len(out) != 45 {
		t.Fatalf("want all 45 LBs across 3 pages, got %d", len(out))
	}
	total := 0
	for _, call := range f.tagCalls {
		if len(call) > 20 {
			t.Fatalf("DescribeTags called with %d ARNs; max is 20", len(call))
		}
		total += len(call)
	}
	if len(f.tagCalls) != 3 || total != 45 {
		t.Fatalf("want 3 batched DescribeTags calls covering 45 ARNs, got %d calls / %d ARNs", len(f.tagCalls), total)
	}
	for _, r := range out {
		if len(r.Ports) != 1 || r.Ports[0] != 443 {
			t.Fatalf("%s: want ports [443], got %v", r.ResourceID, r.Ports)
		}
		if r.Tags == nil {
			t.Fatalf("%s: tags must be a non-nil map", r.ResourceID)
		}
	}
	if out[1].Tags["Name"] != "lb1" {
		t.Fatalf("lb1 tags not attached: %v", out[1].Tags)
	}
	if len(out[0].Tags) != 0 {
		t.Fatalf("lb0 has no tags, got %v", out[0].Tags)
	}
}

func TestCollectLoadBalancersListenerFailureOmitsResourceAndReportsIt(t *testing.T) {
	f := elbFixture(3, 20)
	f.listenerErr = map[string]error{"arn:lb:1": errors.New("Throttling")}
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(out) != 2 {
		t.Fatalf("want lb1 omitted (2 left), got %d", len(out))
	}
	for _, r := range out {
		if r.ResourceID == "arn:lb:1" {
			t.Fatalf("lb1 must be omitted, not sent with partial ports")
		}
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeListeners (r1)") {
		t.Fatalf("listener failure not reported: %v", errs)
	}
}

func TestCollectLoadBalancersTagFailureOmitsThatBatchAndReportsIt(t *testing.T) {
	f := elbFixture(25, 100) // one page of 25 → tag batches of 20 + 5
	f.tagErrOnCall = 1
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(out) != 5 {
		t.Fatalf("want the failed batch of 20 omitted (5 left), got %d", len(out))
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeTags (r1)") {
		t.Fatalf("tag failure not reported: %v", errs)
	}
}

func TestCollectLoadBalancersPageFailureKeepsEarlierPagesAndReportsIt(t *testing.T) {
	f := elbFixture(30, 20) // pages of 20, 10
	f.failPage = map[int]bool{1: true}
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(out) != 20 {
		t.Fatalf("want page 0's 20 LBs kept, got %d", len(out))
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeLoadBalancers (r1)") {
		t.Fatalf("page failure not reported: %v", errs)
	}
}

func rdsPages(n, per int) [][]rdstypes.DBInstance {
	var pages [][]rdstypes.DBInstance
	var cur []rdstypes.DBInstance
	for i := 0; i < n; i++ {
		cur = append(cur, rdstypes.DBInstance{
			DBInstanceArn: aws.String(fmt.Sprintf("arn:db:%d", i)),
			Endpoint:      &rdstypes.Endpoint{Address: aws.String(fmt.Sprintf("db%d.rds", i)), Port: aws.Int32(5432)},
			TagList:       []rdstypes.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
		})
		if len(cur) == per {
			pages, cur = append(pages, cur), nil
		}
	}
	if len(cur) > 0 {
		pages = append(pages, cur)
	}
	return pages
}

func TestCollectRDSReadsAllPagesWithInlineTags(t *testing.T) {
	out, errs := collectRDS(context.Background(), "r1", &fakeRDS{pages: rdsPages(250, 100)}) // 100, 100, 50
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if len(out) != 250 {
		t.Fatalf("want all 250 DB instances across 3 pages, got %d", len(out))
	}
	if out[249].Tags["Env"] != "prod" {
		t.Fatalf("tags must come from TagList, got %v", out[249].Tags)
	}
}

func TestCollectRDSPageFailureKeepsEarlierPagesAndReportsIt(t *testing.T) {
	out, errs := collectRDS(context.Background(), "r1", &fakeRDS{pages: rdsPages(150, 100), failPage: map[int]bool{1: true}})
	if len(out) != 100 {
		t.Fatalf("want page 0's 100 kept, got %d", len(out))
	}
	if !hasErr(errs, "rds:DescribeDBInstances (r1)") {
		t.Fatalf("page failure not reported: %v", errs)
	}
}

func ec2Pages(n, per int) [][]ec2types.Instance {
	var pages [][]ec2types.Instance
	var cur []ec2types.Instance
	for i := 0; i < n; i++ {
		cur = append(cur, ec2types.Instance{InstanceId: aws.String(fmt.Sprintf("i-%d", i)), PrivateIpAddress: aws.String("10.0.0.1")})
		if len(cur) == per {
			pages, cur = append(pages, cur), nil
		}
	}
	if len(cur) > 0 {
		pages = append(pages, cur)
	}
	return pages
}

func TestCollectEC2ReadsAllPages(t *testing.T) {
	out, errs := collectEC2(context.Background(), "r1", &fakeEC2{pages: ec2Pages(12, 5)}) // 5, 5, 2
	if len(errs) != 0 || len(out) != 12 {
		t.Fatalf("want 12 instances and no errs, got %d / %v", len(out), errs)
	}
}

func TestCollectEC2PageFailureKeepsEarlierPagesAndReportsIt(t *testing.T) {
	out, errs := collectEC2(context.Background(), "r1", &fakeEC2{pages: ec2Pages(12, 5), failPage: map[int]bool{2: true}})
	if len(out) != 10 {
		t.Fatalf("want pages 0-1 (10) kept, got %d", len(out))
	}
	if !hasErr(errs, "ec2:DescribeInstances (r1)") {
		t.Fatalf("page failure not reported: %v", errs)
	}
}

func TestCollectRegionCombinesAllServices(t *testing.T) {
	c := awsClients{elb: elbFixture(2, 20), ec2: &fakeEC2{pages: ec2Pages(3, 5)}, rds: &fakeRDS{pages: rdsPages(4, 100)}}
	out, errs := collectRegion(context.Background(), "r1", c)
	if len(errs) != 0 || len(out) != 9 {
		t.Fatalf("want 2+3+4 = 9 resources and no errs, got %d / %v", len(out), errs)
	}
}

func TestAWSErrSaysTimedOutOnDeadline(t *testing.T) {
	err := fmt.Errorf("operation error Elastic Load Balancing v2: DescribeListeners, https response error StatusCode: 0, RequestID: , canceled, %w", context.DeadlineExceeded)
	if got, want := awsErr("elasticloadbalancing:DescribeListeners", "r1", err), "elasticloadbalancing:DescribeListeners (r1): timed out"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCollectRegionReportsADeadlineMidScan(t *testing.T) {
	f := elbFixture(2, 20)
	f.blockListeners = true
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, errs := collectRegion(ctx, "r1", awsClients{elb: f, ec2: &fakeEC2{}, rds: &fakeRDS{}})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("collectRegion did not stop at the deadline")
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeListeners (r1): timed out") {
		t.Fatalf("deadline not reported as timed out: %v", errs)
	}
}

func TestCollectAllRegionsGivesEachRegionItsOwnDeadline(t *testing.T) {
	var fastLive bool
	collect := func(ctx context.Context, region string) ([]CloudResource, []string) {
		if region == "slow" {
			<-ctx.Done() // uses up its whole budget
			return nil, []string{awsErr("ec2:DescribeInstances", region, ctx.Err())}
		}
		fastLive = ctx.Err() == nil
		return []CloudResource{{ResourceID: "fast-1"}}, nil
	}
	out, errs := collectAllRegions([]string{"slow", "fast"}, 50*time.Millisecond, collect)
	if !fastLive {
		t.Fatalf("the region after a slow one started with an expired deadline")
	}
	if len(out) != 1 || !hasErr(errs, "ec2:DescribeInstances (slow): timed out") {
		t.Fatalf("got out=%v errs=%v", out, errs)
	}
}
