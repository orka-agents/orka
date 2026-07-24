import { createFileRoute } from '@tanstack/react-router'
import { GatewayPage } from '@/components/gateways/gateway-page'

export const Route = createFileRoute('/gateways/')({
  component: GatewayPage,
})
