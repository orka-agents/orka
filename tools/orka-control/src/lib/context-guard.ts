export interface CrawlToken {
  context: string;
  generation: number;
}

export class ContextGuard {
  private context = "";
  private generation = 0;

  select(context: string): void {
    this.context = context;
    this.generation += 1;
  }

  current(): string {
    return this.context;
  }

  beginCrawl(context: string): CrawlToken {
    this.generation += 1;
    return { context, generation: this.generation };
  }

  accepts(token: CrawlToken): boolean {
    return token.context === this.context && token.generation === this.generation;
  }

  isCurrent(context: string): boolean {
    return context === this.context;
  }
}
