// The entrypoint the platform runs: `node /bot/main.ts`.
// A thin launcher: exec the vendored static Go binary in this directory.
// The binary owns the whole lifecycle (bind, register, poll /done, serve)
// and inherits this process's environment (MAKEFUN_ENV_URL, MAKEFUN_SHOP_TOKEN).
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const bin = path.join(dir, 'forge');
// GOGC=off removes GC pauses from the hot path (the heap stays small:
// a trial is tens of thousands of requests at a few KB each); GOMEMLIMIT
// is the safety valve if the heap ever grows past 512 MiB.
const child = spawn(bin, [], {
  stdio: 'inherit',
  env: { ...process.env, GOGC: 'off', GOMEMLIMIT: '512MiB' },
});
child.on('error', (err) => {
  process.stderr.write(`[entry] cannot start forge: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal !== null) process.exit(1);
  process.exit(code ?? 1);
});
for (const sig of ['SIGTERM', 'SIGINT']) {
  process.on(sig, () => child.kill(sig));
}
