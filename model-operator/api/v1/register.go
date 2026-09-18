package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SchemeGroupVersion is the group version for this API
var SchemeGroupVersion = schema.GroupVersion{
	Group:   "models.hpe.io",
	Version: "v1",
}

// AddToScheme registers the types in this package with the given scheme
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&ModelDeployment{},
		&ModelDeploymentList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// Resource takes an unqualified resource and returns a Group-qualified GroupResource
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}
