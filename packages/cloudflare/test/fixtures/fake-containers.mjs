export class Container {
  constructor(ctx, env) {
    this.ctx = ctx
    this.env = env
  }

  async fetch(request) {
    return this.containerFetch?.(request) ?? new Response(null, { status: 204 })
  }

  async startAndWaitForPorts() {}

  renewActivityTimeout() {}

  deleteSchedules() {}

  async schedule() {}

  async destroy() {}
}

export class ContainerProxy {}
