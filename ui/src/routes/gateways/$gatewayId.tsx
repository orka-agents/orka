import { createFileRoute } from '@tanstack/react-router'
import { GatewayDetail } from '@/components/gateways/gateway-detail'

export const Route = createFileRoute('/gateways/$gatewayId')({
  component: GatewayDetailRoute,
})

function GatewayDetailRoute() {
  const { gatewayId } = Route.useParams()
  return <GatewayDetail name={gatewayId} />
}
