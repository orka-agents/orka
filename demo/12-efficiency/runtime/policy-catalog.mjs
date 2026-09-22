// Vekil policy routes accept text and standard function tools. Use the same
// restrictions as Vekil's launch/codex_catalog.go, for this demo's one model.
import { readFileSync } from 'node:fs';

const catalog = JSON.parse(readFileSync(0, 'utf8'));
const model = catalog.models.find(item => item.slug === 'gpt-5.5');
if (!model) throw new Error('The pinned Codex catalog must contain gpt-5.5');
Object.assign(model, {
  slug: 'team-assistant',
  display_name: 'Semantic router',
  description: 'Vekil selects the configured local or hosted model.',
  base_instructions: 'You are a coding assistant working in a repository prepared by Orka. Follow the supplied developer instructions and task. Use the available tools to inspect, edit, and test files.',
  model_messages: null,
  include_skills_usage_instructions: false,
  priority: 0,
  visibility: 'list',
  supported_in_api: true,
  availability_nux: null,
  upgrade: null,
  supported_reasoning_levels: [],
  supports_reasoning_summaries: false,
  support_verbosity: false,
  supports_search_tool: false,
  experimental_supported_tools: [],
  apply_patch_tool_type: null,
  use_responses_lite: false,
  additional_speed_tiers: [],
  service_tiers: [],
  supports_parallel_tool_calls: true,
  supports_image_detail_original: false,
  input_modalities: ['text'],
  context_window: 32768,
  max_context_window: 32768,
});
delete model.default_reasoning_level;
delete model.tool_mode;
process.stdout.write(JSON.stringify({ models: [model] }) + '\n');
