import re
with open("internal/extension/marketplace/manager_test.go", "r") as f:
    content = f.read()

# Just replace specific lines that have 6 arguments with 7.
content = content.replace('mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, nil, nil, nil)', 'mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, nil, nil, nil, nil)')
content = content.replace('mgr := NewManager(repo.NewSQLiteExtensionRepository(db), remover, nil, nil, nil, nil)', 'mgr := NewManager(repo.NewSQLiteExtensionRepository(db), remover, nil, nil, nil, nil, nil)')
content = content.replace('mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil)', 'mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil, nil)')
content = content.replace('mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, prefs, nil, limits)', 'mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, prefs, nil, limits, nil)')

with open("internal/extension/marketplace/manager_test.go", "w") as f:
    f.write(content)
