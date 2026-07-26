interface ObservedGenerationResource {
  metadata: { generation?: number }
  status?: { ready?: boolean; observedGeneration?: number }
}

export function isGatewayStatusFresh(resource: ObservedGenerationResource) {
  const generation = resource.metadata.generation
  return typeof generation === 'number'
    && generation > 0
    && resource.status?.observedGeneration === generation
}

export function isGatewayResourceReady(resource: ObservedGenerationResource) {
  return resource.status?.ready === true && isGatewayStatusFresh(resource)
}
