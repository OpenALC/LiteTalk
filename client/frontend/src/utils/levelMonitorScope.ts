// 管理一次设置页实例持有的原生电平监视 token。
// 页面卸载与异步启动完成可能交错，迟到的 token 必须立即归还。
export class LevelMonitorScope {
  private token: string | null = null
  private disposed = false

  constructor(
    private readonly startNative: () => Promise<string | undefined> | undefined,
    private readonly stopNative: (token: string) => Promise<void> | undefined,
  ) {}

  get closed(): boolean { return this.disposed }

  async start(): Promise<boolean> {
    if (this.disposed) return false
    const token = await this.startNative()
    if (this.disposed) {
      if (token) await this.stopNative(token)
      return false
    }
    this.token = token ?? null
    return true
  }

  async stop(): Promise<void> {
    const token = this.token
    this.token = null
    if (token) await this.stopNative(token)
  }

  async dispose(): Promise<void> {
    this.disposed = true
    await this.stop()
  }
}
