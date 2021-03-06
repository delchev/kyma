// Code generated by failery v1.0.0. DO NOT EDIT.

package disabled

import storage "github.com/kyma-project/kyma/components/console-backend-service/internal/domain/content/storage"

// apiSpecGetter is an autogenerated failing mock type for the apiSpecGetter type
type apiSpecGetter struct {
	err error
}

// NewApiSpecGetter creates a new apiSpecGetter type instance
func NewApiSpecGetter(err error) *apiSpecGetter {
	return &apiSpecGetter{err: err}
}

// Find provides a failing mock function with given fields: kind, id
func (_m *apiSpecGetter) Find(kind string, id string) (*storage.ApiSpec, error) {
	var r0 *storage.ApiSpec
	var r1 error
	r1 = _m.err

	return r0, r1
}
