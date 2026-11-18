// The entrypoint the grader runs: `node /bot/main.ts` (Node 22.18+ strips types natively).
//
// This file is a thin launcher: the forge itself is a vendored, statically linked
// linux/amd64 binary (./forge). It owns the whole trial lifecycle exactly as the wire
// contract requires — bind an ephemeral port, POST {"port": N} to $MAKEFUN_ENV_URL/register,
// then long-poll $MAKEFUN_ENV_URL/done until the suite says {"done": true} and exit 0.
// Process signals and stdio pass straight through, so the suite sees the entry as one process.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const bin = join(dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(bin, [], { stdio: 'inherit', env: process.env });

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start ${bin}: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.stderr.write(`[entry] forge exited on signal ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 0);
});