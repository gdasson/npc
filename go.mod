module github.com/myorg/nodepatcher

go 1.22.0

toolchain go1.23.6

require (
	github.com/aws/aws-sdk-go-v2 v1.30.1
	github.com/aws/aws-sdk-go-v2/config v1.27.18
	github.com/aws/aws-sdk-go-v2/service/autoscaling v1.40.0
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.168.0
	k8s.io/api v0.30.3
	k8s.io/apimachinery v0.30.3
	k8s.io/client-go v0.30.3
	k8s.io/kubectl v0.30.3
	sigs.k8s.io/controller-runtime v0.18.4
)
