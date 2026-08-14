import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { PageHeader } from '@/components/layout/page-header'
import { MemoryBrowser } from '@/components/memory/memory-browser'
import { ProposalInbox } from '@/components/memory/proposal-inbox'
import { useMemoryProposalList } from '@/hooks/use-memory'

export function MemoryPage() {
  // Pending proposals surface as a count on the tab so reviews don't stall.
  const { data: pending } = useMemoryProposalList({ status: 'pending' })
  const pendingCount = pending?.items.length ?? 0

  return (
    <div className="space-y-4">
      <PageHeader
        title="Memory"
        description="Durable memory is governance-first: agents propose, humans review, applying creates the memory."
      />
      <Tabs defaultValue="memories">
        <TabsList>
          <TabsTrigger value="memories">Memories</TabsTrigger>
          <TabsTrigger value="proposals">
            Proposals
            {pendingCount > 0 && (
              <span className="ml-1.5 rounded-full bg-primary/15 px-1.5 text-xs font-semibold text-primary">
                {pendingCount}
              </span>
            )}
          </TabsTrigger>
        </TabsList>
        <TabsContent value="memories" className="space-y-4">
          <MemoryBrowser />
        </TabsContent>
        <TabsContent value="proposals" className="space-y-4">
          <ProposalInbox />
        </TabsContent>
      </Tabs>
    </div>
  )
}
