import re
with open("internal/extension/marketplace/manager_test.go", "r") as f:
    content = f.read()

content = content.replace('NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate, mockPrefs, nil, nil)', 'NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate, mockPrefs, nil, nil, nil)')

with open("internal/extension/marketplace/manager_test.go", "w") as f:
    f.write(content)
