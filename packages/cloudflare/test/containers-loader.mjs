const fakeContainersUrl = new URL('./fixtures/fake-containers.mjs', import.meta.url).href

export async function resolve(specifier, context, nextResolve) {
  if (specifier === '@cloudflare/containers') return { url: fakeContainersUrl, shortCircuit: true }
  return nextResolve(specifier, context)
}
