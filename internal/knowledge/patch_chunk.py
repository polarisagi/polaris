import re

with open('internal/knowledge/rag.go', 'r') as f:
    rag_content = f.read()

chunk_pattern = re.compile(r'// Chunk is a retrievable unit.*?type Chunk struct \{.*?\n\}\n', re.DOTALL)
chunk_match = chunk_pattern.search(rag_content)
if chunk_match:
    chunk_code = chunk_match.group(0)
    
    # Write to doc_types.go
    with open('internal/knowledge/graphrag/doc_types.go', 'r') as f:
        doc_content = f.read()
    
    new_doc_content = doc_content + "\n" + chunk_code
    with open('internal/knowledge/graphrag/doc_types.go', 'w') as f:
        f.write(new_doc_content)
    
    # Replace in rag.go
    new_rag_content = rag_content.replace(chunk_code, '// Chunk is aliased to graphrag.Chunk to prevent import cycles.\ntype Chunk = graphrag.Chunk\n')
    
    # add import graphrag if not there
    if '"github.com/polarisagi/polaris/internal/knowledge/graphrag"' not in new_rag_content:
        new_rag_content = new_rag_content.replace('import (', 'import (\n\t"github.com/polarisagi/polaris/internal/knowledge/graphrag"\n', 1)
        
    with open('internal/knowledge/rag.go', 'w') as f:
        f.write(new_rag_content)
