import os
import glob

def fix_file(filepath):
    with open(filepath, 'r') as f:
        content = f.read()

    # extension_instances
    if "CREATE TABLE IF NOT EXISTS extension_instances" in content or "CREATE TABLE extension_instances" in content:
        if "installed_version TEXT" not in content:
            content = content.replace("name TEXT,", "name TEXT,\n\t\t\tinstalled_version TEXT,")
            content = content.replace("name         TEXT    NOT NULL,", "name         TEXT    NOT NULL,\n    installed_version TEXT NOT NULL DEFAULT '',")
            
    # extension_catalog
    if "CREATE TABLE IF NOT EXISTS extension_catalog" in content or "CREATE TABLE extension_catalog" in content:
        if "version TEXT" not in content and "version        TEXT" not in content:
            content = content.replace("url TEXT,", "url TEXT,\n\t\t\tversion TEXT,")
            content = content.replace("url            TEXT NOT NULL DEFAULT '',", "url            TEXT NOT NULL DEFAULT '',\n    version        TEXT NOT NULL DEFAULT '',")

    with open(filepath, 'w') as f:
        f.write(content)

for root, dirs, files in os.walk("internal"):
    for file in files:
        if file.endswith("_test.go"):
            fix_file(os.path.join(root, file))
