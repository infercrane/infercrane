package providerfixture

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// AWSCLI simulates only the narrow AWS CLI calls made by provision.AWSEC2.
// It is deterministic and never contacts AWS.
type AWSCLI struct {
	InstanceID, ExternalKey, State string
	FailAfterCreateOnce            bool
	CreateCalls, DeleteCalls       int
}

func (f *AWSCLI) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "sts" && args[1] == "assume-role" {
		return []byte(`{"Credentials":{"AccessKeyId":"fixture-access","SecretAccessKey":"fixture-secret","SessionToken":"fixture-token"}}`), nil
	}
	if len(args) < 2 || args[0] != "ec2" {
		return nil, errors.New("unexpected fixture command")
	}
	switch args[1] {
	case "describe-instances":
		if f.InstanceID == "" {
			return []byte(`{"Reservations":[]}`), nil
		}
		response := map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{"InstanceId": f.InstanceID, "ImageId": "ami-gpu", "InstanceType": "g6e.xlarge", "SubnetId": "subnet-private", "PrivateIpAddress": "10.0.0.8", "IamInstanceProfile": map[string]string{"Arn": "arn:aws:iam::123456789012:instance-profile/worker"}, "SecurityGroups": []map[string]string{{"GroupId": "sg-worker"}}, "State": map[string]string{"Name": f.State}, "Tags": []map[string]string{{"Key": "infercrane:external-key", "Value": f.ExternalKey}, {"Key": "infercrane:root-volume-gib", "Value": "100"}, {"Key": "infercrane:root-volume-encrypted", "Value": "true"}}}}}}}
		return json.Marshal(response)
	case "run-instances":
		f.CreateCalls++
		f.InstanceID, f.State = "i-conformance", "running"
		for i, arg := range args {
			if arg == "--tag-specifications" && i+1 < len(args) {
				const marker = `"Key":"infercrane:external-key","Value":"`
				if start := strings.Index(args[i+1], marker); start >= 0 {
					value := args[i+1][start+len(marker):]
					f.ExternalKey = strings.SplitN(value, `"`, 2)[0]
				}
			}
		}
		if f.FailAfterCreateOnce {
			f.FailAfterCreateOnce = false
			return nil, errors.New("injected lost AWS create response")
		}
		return []byte(`{"Instances":[{"InstanceId":"i-conformance"}]}`), nil
	case "terminate-instances":
		f.DeleteCalls++
		f.State = "terminated"
		return []byte(`{"TerminatingInstances":[{"InstanceId":"i-conformance"}]}`), nil
	default:
		return nil, errors.New("unexpected fixture EC2 command")
	}
}
