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
	tagFailArns    map[string]bool              // any DescribeTags call that includes one of these ARNs fails
	tagCalls       [][]string
	listenerCalls  int
	sameMarker     bool // DescribeLoadBalancers/DescribeListeners always return the same next token
	emptyEnd       bool // the last page carries an EMPTY next token (the SDK treats that as the end)
}

func (f *fakeELB) DescribeLoadBalancers(ctx context.Context, in *elbv2.DescribeLoadBalancersInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeLoadBalancersOutput, error) {
	items, next, i := page(f.pages, in.Marker)
	if f.failPage[i] {
		return nil, errors.New("Throttling: Rate exceeded")
	}
	if f.sameMarker {
		next = aws.String("same")
	}
	if f.emptyEnd && next == nil {
		next = aws.String("")
	}
	return &elbv2.DescribeLoadBalancersOutput{LoadBalancers: items, NextMarker: next}, nil
}

func (f *fakeELB) DescribeListeners(ctx context.Context, in *elbv2.DescribeListenersInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeListenersOutput, error) {
	f.listenerCalls++
	if f.sameMarker {
		return &elbv2.DescribeListenersOutput{NextMarker: aws.String("same")}, nil
	}
	if f.emptyEnd {
		return &elbv2.DescribeListenersOutput{Listeners: []elbtypes.Listener{{Port: aws.Int32(443)}}, NextMarker: aws.String("")}, nil
	}
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
	for _, a := range in.ResourceArns {
		if f.tagFailArns[a] {
			return nil, errors.New("AccessDenied")
		}
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
	pages     [][]ec2types.Instance
	failPage  map[int]bool
	sameToken bool // always return the same next token
	emptyEnd  bool // the last page carries an empty next token
	calls     int
}

func (f *fakeEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.calls++
	items, next, i := page(f.pages, in.NextToken)
	if f.failPage[i] {
		return nil, errors.New("RequestLimitExceeded")
	}
	if f.sameToken {
		next = aws.String("same")
	}
	if f.emptyEnd && next == nil {
		next = aws.String("")
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: items}}, NextToken: next}, nil
}

type fakeRDS struct {
	pages     [][]rdstypes.DBInstance
	failPage  map[int]bool
	sameToken bool // always return the same next token
	emptyEnd  bool // the last page carries an empty next token
	calls     int
}

func (f *fakeRDS) DescribeDBInstances(ctx context.Context, in *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	f.calls++
	items, next, i := page(f.pages, in.Marker)
	if f.failPage[i] {
		return nil, errors.New("Throttling")
	}
	if f.sameToken {
		next = aws.String("same")
	}
	if f.emptyEnd && next == nil {
		next = aws.String("")
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

func TestCollectLoadBalancersRecoversFromATransientTagBatchFailure(t *testing.T) {
	f := elbFixture(25, 100) // one page of 25 → tag batches of 20 + 5
	f.tagErrOnCall = 1       // only the first batch call fails; the per-ARN retries succeed
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(out) != 25 || len(errs) != 0 {
		t.Fatalf("a failed batch is retried per ARN: want all 25 and no errors, got %d / %v", len(out), errs)
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
	out, errs, _ := collectRegion(context.Background(), "r1", c)
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
	_, errs, _ := collectRegion(ctx, "r1", awsClients{elb: f, ec2: &fakeEC2{}, rds: &fakeRDS{}})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("collectRegion did not stop at the deadline")
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeListeners (r1): timed out") {
		t.Fatalf("deadline not reported as timed out: %v", errs)
	}
}

func TestCollectAllRegionsGivesEachRegionItsOwnDeadline(t *testing.T) {
	var fastLive bool
	collect := func(ctx context.Context, region string) ([]CloudResource, []string, []ScannedPair) {
		if region == "slow" {
			<-ctx.Done() // uses up its whole budget
			return nil, []string{awsErr("ec2:DescribeInstances", region, ctx.Err())}, nil
		}
		fastLive = ctx.Err() == nil
		return []CloudResource{{ResourceID: "fast-1"}}, nil, []ScannedPair{{ResourceType: "ec2", Region: region}}
	}
	out, errs, scanned := collectAllRegions([]string{"slow", "fast"}, 50*time.Millisecond, collect)
	if !fastLive {
		t.Fatalf("the region after a slow one started with an expired deadline")
	}
	if len(out) != 1 || !hasErr(errs, "ec2:DescribeInstances (slow): timed out") {
		t.Fatalf("got out=%v errs=%v", out, errs)
	}
	if len(scanned) != 1 || scanned[0].Region != "fast" {
		t.Fatalf("only the region that finished is a scanned pair, got %v", scanned)
	}
}

func hasPair(pairs []ScannedPair, rt, region string) bool {
	for _, p := range pairs {
		if p.ResourceType == rt && p.Region == region {
			return true
		}
	}
	return false
}

func TestCollectRegionReportsEveryCleanPairAsScanned(t *testing.T) {
	c := awsClients{elb: elbFixture(2, 20), ec2: &fakeEC2{pages: ec2Pages(3, 5)}, rds: &fakeRDS{pages: rdsPages(4, 100)}}
	_, errs, scanned := collectRegion(context.Background(), "r1", c)
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	for _, rt := range []string{"alb", "nlb", "ec2", "rds"} {
		if !hasPair(scanned, rt, "r1") {
			t.Fatalf("clean %s scan must be reported as scanned, got %v", rt, scanned)
		}
	}
}

func TestCollectRegionLeavesAFailedServiceOutOfScanned(t *testing.T) {
	// RDS is denied (a persistent partial error); load balancers and EC2 are fine.
	c := awsClients{elb: elbFixture(2, 20), ec2: &fakeEC2{pages: ec2Pages(3, 5)}, rds: &fakeRDS{pages: rdsPages(4, 100), failPage: map[int]bool{0: true}}}
	_, errs, scanned := collectRegion(context.Background(), "r1", c)
	if !hasErr(errs, "rds:DescribeDBInstances (r1)") {
		t.Fatalf("rds failure not reported: %v", errs)
	}
	if hasPair(scanned, "rds", "r1") {
		t.Fatalf("a failed rds scan must not be reported as scanned: %v", scanned)
	}
	for _, rt := range []string{"alb", "nlb", "ec2"} {
		if !hasPair(scanned, rt, "r1") {
			t.Fatalf("%s scanned cleanly and must still be reported: %v", rt, scanned)
		}
	}
}

func TestCollectRegionLoadBalancerFailureLeavesAlbAndNlbOutOfScanned(t *testing.T) {
	f := elbFixture(3, 20)
	f.listenerErr = map[string]error{"arn:lb:1": errors.New("Throttling")}
	_, _, scanned := collectRegion(context.Background(), "r1", awsClients{elb: f, ec2: &fakeEC2{}, rds: &fakeRDS{}})
	if hasPair(scanned, "alb", "r1") || hasPair(scanned, "nlb", "r1") {
		t.Fatalf("an LB that was left out means the LB scan is not complete: %v", scanned)
	}
	if !hasPair(scanned, "ec2", "r1") || !hasPair(scanned, "rds", "r1") {
		t.Fatalf("ec2/rds were clean: %v", scanned)
	}
}

func TestCollectLoadBalancersStopsAtTheDeadlineInsteadOfFloodingErrors(t *testing.T) {
	f := elbFixture(30, 100)
	f.blockListeners = true
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, errs := collectLoadBalancers(ctx, "r1", f)
	if f.listenerCalls != 1 {
		t.Fatalf("after the deadline no further listener calls should be made, got %d", f.listenerCalls)
	}
	if len(errs) != 1 || !hasErr(errs, "DescribeListeners (r1): timed out") {
		t.Fatalf("want exactly one timed-out error, got %v", errs)
	}
}

func TestPaginatorsStopOnARepeatedTokenAndReportIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	e := &fakeEC2{pages: ec2Pages(10, 5), sameToken: true}
	_, errs := collectEC2(ctx, "r1", e)
	if e.calls > 3 || !hasErr(errs, "ec2:DescribeInstances (r1): repeated page token") {
		t.Fatalf("ec2: %d calls, errs %v", e.calls, errs)
	}

	r := &fakeRDS{pages: rdsPages(250, 100), sameToken: true}
	_, errs = collectRDS(ctx, "r1", r)
	if r.calls > 3 || !hasErr(errs, "rds:DescribeDBInstances (r1): repeated page token") {
		t.Fatalf("rds: %d calls, errs %v", r.calls, errs)
	}

	f := elbFixture(30, 20)
	f.sameMarker = true
	_, errs = collectLoadBalancers(ctx, "r1", f)
	if !hasErr(errs, "elasticloadbalancing:DescribeLoadBalancers (r1): repeated page token") {
		t.Fatalf("lb: errs %v", errs)
	}
	if f.listenerCalls > 100 {
		t.Fatalf("listener paginator kept spinning: %d calls", f.listenerCalls)
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeListeners (r1): repeated page token") {
		t.Fatalf("listeners: errs %v", errs)
	}
}

func TestCollectLoadBalancersRetriesEachARNWhenATagBatchFails(t *testing.T) {
	f := elbFixture(25, 100) // tag batches of 20 + 5
	f.tagFailArns = map[string]bool{"arn:lb:3": true}
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(out) != 24 {
		t.Fatalf("only the one LB whose tags can't be read is left out, got %d of 25", len(out))
	}
	for _, r := range out {
		if r.ResourceID == "arn:lb:3" {
			t.Fatalf("lb3 must be omitted")
		}
	}
	if !hasErr(errs, "elasticloadbalancing:DescribeTags (r1)") {
		t.Fatalf("the failing ARN must still be reported: %v", errs)
	}
	if len(errs) != 1 {
		t.Fatalf("one failing ARN is one error (not one per batch member): %v", errs)
	}
}

// The SDK treats an EMPTY next token as the end of the listing. A final page that carries one must not be
// reported as a repeated-token loop (that would drop the service from `scanned` on every scan).
func TestAnEmptyFinalPageTokenIsTheEndNotARepeatedToken(t *testing.T) {
	e := &fakeEC2{pages: ec2Pages(7, 5), emptyEnd: true}
	if out, errs := collectEC2(context.Background(), "r1", e); len(errs) != 0 || len(out) != 7 {
		t.Fatalf("ec2: want 7 and no errs, got %d / %v", len(out), errs)
	}
	r := &fakeRDS{pages: rdsPages(150, 100), emptyEnd: true}
	if out, errs := collectRDS(context.Background(), "r1", r); len(errs) != 0 || len(out) != 150 {
		t.Fatalf("rds: want 150 and no errs, got %d / %v", len(out), errs)
	}
	f := elbFixture(30, 20)
	f.emptyEnd = true
	out, errs := collectLoadBalancers(context.Background(), "r1", f)
	if len(errs) != 0 || len(out) != 30 {
		t.Fatalf("elb + listeners: want 30 and no errs, got %d / %v", len(out), errs)
	}
	if ports, err := listenerPorts(context.Background(), f, aws.String("arn:lb:0")); err != nil || len(ports) != 1 {
		t.Fatalf("listeners: want 1 port and no error, got %v / %v", ports, err)
	}
}
