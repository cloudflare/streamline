import assert from 'node:assert/strict'
import { execFile as execFileCallback } from 'node:child_process'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import test from 'node:test'

const execFile = promisify(execFileCallback)
const packageDirectory = fileURLToPath(new URL('..', import.meta.url))
const containersDirectory = join(packageDirectory, 'node_modules/@cloudflare/containers')
const workersTypesDirectory = join(packageDirectory, 'node_modules/@cloudflare/workers-types')
const typescriptCli = join(packageDirectory, 'node_modules/typescript/lib/tsc.js')

async function run(file, args, cwd) {
  return execFile(file, args, { cwd, maxBuffer: 10 * 1024 * 1024 })
}

test('packed package installs and typechecks in an isolated consumer', async (context) => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), 'streamline-cloudflare-'))
  context.after(() => rm(temporaryDirectory, { force: true, recursive: true }))

  const { stdout } = await run('npm', ['pack', '--json', '--pack-destination', temporaryDirectory], packageDirectory)
  const [packed] = JSON.parse(stdout)
  const paths = packed.files.map(({ path }) => path)
  assert.ok(paths.includes('README.md'))
  assert.ok(paths.includes('package.json'))
  assert.ok(paths.includes('dist/index.js'))
  assert.ok(paths.includes('dist/session-do.d.ts'))
  assert.ok(paths.every((path) => path === 'README.md' || path === 'package.json' || path.startsWith('dist/')))

  const consumerDirectory = join(temporaryDirectory, 'consumer')
  const tarball = join(temporaryDirectory, packed.filename)
  await mkdir(consumerDirectory)
  await writeFile(join(consumerDirectory, 'package.json'), JSON.stringify({
    name: 'streamline-cloudflare-consumer',
    private: true,
    type: 'module',
    dependencies: {
      '@cloudflare/containers': `file:${containersDirectory}`,
      '@cloudflare/streamline': `file:${tarball}`,
    },
    devDependencies: {
      '@cloudflare/workers-types': `file:${workersTypesDirectory}`,
    },
  }))
  await writeFile(join(consumerDirectory, 'client.mjs'), `
    import assert from 'node:assert/strict'
    import { createStreamline } from '@cloudflare/streamline/client'

    const media = createStreamline({ baseUrl: 'https://media.example' })
    assert.equal(typeof media.sessions.create, 'function')
  `)
  await writeFile(join(consumerDirectory, 'worker.ts'), `
    import { ContainerProxy, StreamlineSessionDO } from '@cloudflare/streamline'

    interface Environment {
      ALLOWED_ORIGINS: string
      MAX_SESSION_DURATION_SECONDS: string
    }

    export class MediaSession extends StreamlineSessionDO<Environment> {
      protected async resolveStartConfig(_request: Request, body: Record<string, unknown>) {
        return { body, maxSessionSeconds: 60 }
      }
    }

    export { ContainerProxy }
  `)
  await writeFile(join(consumerDirectory, 'tsconfig.json'), JSON.stringify({
    compilerOptions: {
      lib: ['ESNext'],
      module: 'NodeNext',
      moduleResolution: 'NodeNext',
      noEmit: true,
      strict: true,
      target: 'ES2024',
      types: ['@cloudflare/workers-types'],
    },
    include: ['worker.ts'],
  }))

  await run('npm', ['install', '--ignore-scripts', '--no-package-lock'], consumerDirectory)
  await run(process.execPath, ['client.mjs'], consumerDirectory)
  await run(process.execPath, [typescriptCli, '--project', 'tsconfig.json'], consumerDirectory)
})
