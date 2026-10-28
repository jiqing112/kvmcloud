// install.sh 静态平衡检查：引号配对、case/esac、if/fi、大括号计数。
// 只读分析，不执行脚本。路径安全：目标文件名固定为仓库根的安装脚本，
// 校验文件存在后仅读内存分析，无任何文件写入。
const fs = require('fs');
const path = require('path');

const repoRoot = path.resolve(__dirname, path.join('..'));
const target = path.join(repoRoot, 'install.sh');
if (!fs.existsSync(target) || !fs.statSync(target).isFile()) {
  console.error('install.sh not found in repo root:', target);
  process.exit(2);
}
const src = fs.readFileSync(target, 'utf8');
const lines = src.split('\n');
const errs = [];

// 1) 引号逐行平衡（忽略注释内与反斜杠转义）
lines.forEach((ln, idx) => {
  let inDQ = false, inSQ = false, prev = '';
  for (let i = 0; i < ln.length; i++) {
    const c = ln[i];
    if (prev !== '\\') {
      if (!inSQ && c === '"') inDQ = !inDQ;
      else if (!inDQ && c === "'") inSQ = !inSQ;
      else if (!inDQ && !inSQ && c === '#') break; // 注释
    }
    prev = c;
  }
  if (inDQ || inSQ) errs.push(`line ${idx + 1}: unclosed quote`);
});

// 2) 块结构配对（词法层）
const tokens = src
  .replace(/#[^\n]*/g, '')
  .replace(/(["'])(?:\\.|(?!\1)[^\\])*\1/g, "''")
  .split(/\s+/);
let ifDepth = 0, caseDepth = 0;
for (const t of tokens) {
  if (t === 'if') ifDepth++;
  else if (t === 'fi') ifDepth--;
  else if (t === 'case') caseDepth++;
  else if (t === 'esac') caseDepth--;
}
if (ifDepth !== 0) errs.push(`unbalanced if/fi (delta ${ifDepth})`);
if (caseDepth !== 0) errs.push(`unbalanced case/esac (delta ${caseDepth})`);

// 3) 大括号计数（排除参数展开语法）
const stripped = src.replace(/\$\{[^}]*\}/g, '');
const open = (stripped.match(/{/g) || []).length;
const close = (stripped.match(/}/g) || []).length;
if (open !== close) errs.push(`brace count {=${open} }=${close}`);

console.log(errs.length ? 'CHECK_ERRORS: ' + errs.join('; ') : 'STATIC_CHECK_OK');
