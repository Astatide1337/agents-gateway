package objectstore_test

import (
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
)

var _ artifacts.Store = (*objectstore.Store)(nil)
var _ effects.ObjectStore = (*objectstore.Store)(nil)
