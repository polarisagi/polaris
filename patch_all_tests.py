import os
import glob
import re

for root, dirs, files in os.walk("internal"):
    for file in files:
        if file.endswith("_test.go"):
            path = os.path.join(root, file)
            with open(path, "r") as f:
                content = f.read()
            original = content
            
            # Fix repo.NewSQLiteExtensionRepository
            content = content.replace("repo.NewSQLiteExtensionRepository(db, nil)", "repo.NewSQLiteExtensionRepository(db)")
            
            # Fix NewManager missing arg
            # Just look for any NewManager call that ends with nil, nil) or whatever
            # and count arguments or just blindly replace the exact strings
            content = re.sub(r'marketplace\.NewManager\((repo\.NewSQLiteExtensionRepository\(db\)), (.*?), (.*?), (.*?), (.*?), (.*?)\)', r'marketplace.NewManager(\1, \2, \3, \4, \5, \6, nil)', content)
            
            content = re.sub(r'NewManager\((repo\.NewSQLiteExtensionRepository\(db\)), (.*?), (.*?), (.*?), (.*?), (.*?)\)', r'NewManager(\1, \2, \3, \4, \5, \6, nil)', content)
            
            if original != content:
                print(f"Patched {path}")
                with open(path, "w") as f:
                    f.write(content)

