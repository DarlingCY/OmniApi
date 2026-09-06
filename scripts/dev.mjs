import { spawn } from 'node:child_process';

const command = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const spawnOptions = { stdio: 'inherit', shell: process.platform === 'win32' };
const children = [
  spawn(command, ['run', 'dev:web'], spawnOptions),
  spawn(command, ['run', 'dev:go'], spawnOptions),
];

let stopping = false;
function stop(exitCode = 0) {
  if (stopping) return;
  stopping = true;
  for (const child of children) child.kill();
  process.exitCode = exitCode;
}

for (const child of children) child.on('exit', (code) => stop(code ?? 0));
process.on('SIGINT', () => stop());
process.on('SIGTERM', () => stop());