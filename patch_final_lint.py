import re
with open("internal/gateway/server/server_init_test.go", "r") as f:
    content = f.read()
content = content.replace("marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil, nil, nil)", "marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil, nil)")
with open("internal/gateway/server/server_init_test.go", "w") as f:
    f.write(content)

with open("internal/gateway/server/handler_codeact.go", "r") as f:
    content = f.read()
content = content.replace('\t"github.com/polarisagi/polaris/pkg/types"\n', '')
with open("internal/gateway/server/handler_codeact.go", "w") as f:
    f.write(content)

with open("internal/gateway/server/server_lifecycle.go", "r") as f:
    content = f.read()
content = content.replace('\t"github.com/polarisagi/polaris/internal/security/taint"\n', '')
with open("internal/gateway/server/server_lifecycle.go", "w") as f:
    f.write(content)
