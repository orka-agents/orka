import { createFileRoute } from '@tanstack/react-router'
import { GatewayBindingDetail } from '@/components/gateways/gateway-binding-detail'

export const Route = createFileRoute('/gateways/bindings/$bindingId')({
  component: GatewayBindingDetailRoute,
})

function GatewayBindingDetailRoute() {
  const { bindingId } = Route.useParams()
  return <GatewayBindingDetail name={bindingId} />
}
