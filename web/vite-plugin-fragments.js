
import fs from 'fs'
import path from 'path'

export default function htmlFragments() {
  return {
    name: 'html-fragments',
    enforce: 'pre',
    transformIndexHtml(html, ctx) {
      return html.replace(/<page-fragment src="([^"]+)"><\/page-fragment>/g, (match, src) => {
        // relative to project root where vite is running
        const filePath = path.resolve('src', src.replace(/^\//, ''));
        try {
          return fs.readFileSync(filePath, 'utf-8');
        } catch (e) {
          console.error(`Failed to read fragment: ${filePath}`);
          return match;
        }
      });
    }
  }
}
