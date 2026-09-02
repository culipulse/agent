package main

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func TestNormalizeLoadBalancer(t *testing.T) {
	lb := elbtypes.LoadBalancer{
		LoadBalancerArn: aws.String("arn:lb:1"),
		DNSName:         aws.String("web.elb.amazonaws.com"),
		Type:            elbtypes.LoadBalancerTypeEnumApplication,
		Scheme:          elbtypes.LoadBalancerSchemeEnumInternetFacing,
		State:           &elbtypes.LoadBalancerState{Code: elbtypes.LoadBalancerStateEnumActive},
	}
	tags := map[string]string{"Env": "prod", "Name": "web"}
	got := normalizeLoadBalancer(lb, []int{443, 80}, tags, "ap-southeast-1")
	want := CloudResource{
		Provider: "aws", ResourceType: "alb", ResourceID: "arn:lb:1", Address: "web.elb.amazonaws.com",
		Ports: []int{443, 80}, Tags: tags,
		Meta: map[string]string{"region": "ap-southeast-1", "scheme": "internet-facing", "state": "active"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestNormalizeLoadBalancerNetworkType(t *testing.T) {
	lb := elbtypes.LoadBalancer{
		LoadBalancerArn: aws.String("arn:lb:2"), DNSName: aws.String("n.elb"),
		Type: elbtypes.LoadBalancerTypeEnumNetwork,
	}
	if got := normalizeLoadBalancer(lb, nil, nil, "r"); got.ResourceType != "nlb" {
		t.Fatalf("want nlb got %s", got.ResourceType)
	}
}

func TestNormalizeEC2InstancePrefersPublic(t *testing.T) {
	inst := ec2types.Instance{
		InstanceId:       aws.String("i-1"),
		PublicIpAddress:  aws.String("1.2.3.4"),
		PrivateIpAddress: aws.String("10.0.0.5"),
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		VpcId:            aws.String("vpc-1"),
		Tags:             []ec2types.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
	}
	got := normalizeEC2Instance(inst, "ap-southeast-1")
	if got.Address != "1.2.3.4" {
		t.Fatalf("want public ip, got %s", got.Address)
	}
	if got.ResourceType != "ec2" || got.ResourceID != "i-1" || got.Tags["Env"] != "prod" {
		t.Fatalf("bad normalize: %+v", got)
	}
	if got.Meta["region"] != "ap-southeast-1" || got.Meta["state"] != "running" || got.Meta["public"] != "true" {
		t.Fatalf("bad meta: %+v", got.Meta)
	}
}

func TestNormalizeEC2InstanceFallsBackToPrivate(t *testing.T) {
	inst := ec2types.Instance{
		InstanceId: aws.String("i-2"), PrivateIpAddress: aws.String("10.0.0.9"),
		State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}
	got := normalizeEC2Instance(inst, "r")
	if got.Address != "10.0.0.9" || got.Meta["public"] != "false" {
		t.Fatalf("want private fallback, got addr=%s public=%s", got.Address, got.Meta["public"])
	}
}

func TestNormalizeRDS(t *testing.T) {
	db := rdstypes.DBInstance{
		DBInstanceArn:        aws.String("arn:rds:1"),
		DBInstanceIdentifier: aws.String("orders"),
		Endpoint:             &rdstypes.Endpoint{Address: aws.String("orders.rds.amazonaws.com"), Port: aws.Int32(5432)},
		Engine:               aws.String("postgres"),
		DBInstanceStatus:     aws.String("available"),
	}
	tags := map[string]string{"Team": "payments"}
	got := normalizeRDS(db, tags, "ap-southeast-1")
	if got.ResourceType != "rds" || got.ResourceID != "arn:rds:1" || got.Address != "orders.rds.amazonaws.com" {
		t.Fatalf("bad normalize: %+v", got)
	}
	if len(got.Ports) != 1 || got.Ports[0] != 5432 || got.Meta["engine"] != "postgres" {
		t.Fatalf("bad ports/meta: %+v", got)
	}
}

func TestRepresentativeError(t *testing.T) {
	if got := representativeError(nil); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	if got := representativeError([]string{"a"}); got != "a" {
		t.Fatalf("single: got %q", got)
	}
	if got := representativeError([]string{"a", "b", "c"}); got != "a (+2 more)" {
		t.Fatalf("multi: got %q", got)
	}
}

func TestParseRegions(t *testing.T) {
	if got := parseRegions("us-east-1, ap-southeast-1 ", "x"); !reflect.DeepEqual(got, []string{"us-east-1", "ap-southeast-1"}) {
		t.Fatalf("got %v", got)
	}
	if got := parseRegions("", "ap-southeast-1"); !reflect.DeepEqual(got, []string{"ap-southeast-1"}) {
		t.Fatalf("fallback failed: %v", got)
	}
	if got := parseRegions("", ""); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}
