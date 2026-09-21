-- Resolve's embedded Lua entry point. The Python preflight writes a driver with
-- resolved media paths and frame counts, so this script needs no JSON package.

local function require_value(value, message)
    if not value then error(message, 2) end
    return value
end

local function json_string(value)
    return '"' .. value:gsub('[%z\1-\31\\"]', function(character)
        if character == '"' then return '\\"' end
        if character == '\\' then return '\\\\' end
        return string.format('\\u%04x', string.byte(character))
    end) .. '"'
end

local function json(value)
    local kind = type(value)
    if kind == 'nil' then return 'null' end
    if kind == 'boolean' or kind == 'number' then return tostring(value) end
    if kind == 'string' then return json_string(value) end
    require_value(kind == 'table', 'Unsupported report value: ' .. kind)
    local array = #value > 0
    local parts = {}
    if array then
        for _, child in ipairs(value) do parts[#parts + 1] = json(child) end
        return '[' .. table.concat(parts, ',') .. ']'
    end
    for key, child in pairs(value) do
        parts[#parts + 1] = json_string(tostring(key)) .. ':' .. json(child)
    end
    table.sort(parts)
    return '{' .. table.concat(parts, ',') .. '}'
end

local function write_report(path, report)
    -- App Store Resolve removes Lua's io library. The external validator
    -- combines the prepared manifest with the actual Resolve exports.
    _orka_last_report = report
    if not io then return end
    local file = require_value(io.open(path, 'w'), 'Could not write report: ' .. path)
    file:write(json(report), '\n')
    file:close()
end

local function exists(path)
    if not io then return bmd.fileexists(path) end
    local file = io.open(path, 'rb')
    if file then file:close(); return true end
    return false
end

local function append_clip(pool, timeline, media, clip, media_type, track)
    local items = pool:AppendToTimeline({{
        mediaPoolItem = media,
        startFrame = clip.source_start_frame,
        -- The installed Resolve 21.1 AppendClipInfo uses an exclusive end.
        endFrame = clip.source_start_frame + clip.frames,
        mediaType = media_type,
        trackIndex = track,
        recordFrame = timeline:GetStartFrame() + clip.record_frame,
    }})
    require_value(items and #items == 1, 'Could not append ' .. clip.path)
    local item = items[1]
    require_value(item:GetDuration() == clip.frames,
                  'Unexpected inserted duration ' .. tostring(item:GetDuration()) ..
                  ' (expected ' .. tostring(clip.frames) .. '): ' .. clip.path)
    require_value(item:GetStart() == timeline:GetStartFrame() + clip.record_frame,
                  'Unexpected inserted position: ' .. clip.path)
end

local function assemble(manifest)
    require_value(manifest.fps == 30 and manifest.width == 1920 and manifest.height == 1080,
                  'The preflight must use 1920x1080 at 30fps')
    require_value(manifest.total_frames > 0 and manifest.total_frames <= (manifest.max_frames or 3570),
                  'The video exceeds the checked duration limit')
    local app = require_value(resolve or bmd.scriptapp('Resolve'), 'Resolve is not available')
    local manager = app:GetProjectManager()
    local previous = manager:GetCurrentProject()
    local base = manifest.output_dir .. '/' .. manifest.output_name
    for _, extension in ipairs({'.mp4', '.drp', '.report.json'}) do
        require_value(not exists(base .. extension), 'Output already exists: ' .. base .. extension)
    end
    local project_name = manifest.resolved_project_name or
        ((manifest.project_name or 'Orka Hackathon First Pass') .. ' ' .. os.date('%Y%m%d-%H%M%S'))
    local report = {
        status = 'creating', project_name = project_name,
        previous_project = previous and previous:GetName() or nil,
        resolve_version = app:GetVersionString(), manifest = manifest,
    }
    local report_path = base .. '.report.json'
    write_report(report_path, report)
    local ok, problem = pcall(function()
        local project = require_value(manager:CreateProject(project_name), 'Could not create a new project')
        for _, setting in ipairs({
            {'timelineFrameRate', '30'},
            {'timelineResolutionWidth', '1920'}, {'timelineResolutionHeight', '1080'},
        }) do
            require_value(project:SetSettings({[setting[1]] = setting[2]}),
                          'Rejected project setting: ' .. setting[1])
        end
        app:OpenPage('edit')
        local pool = project:GetMediaPool()
        local imported = {}
        for _, clips in ipairs({manifest.scenes, manifest.audio}) do
            for _, clip in ipairs(clips) do
                if not imported[clip.path] then
                    local media = app:GetMediaStorage():AddItemsToMediaPool(clip.path)
                    require_value(media and #media == 1, 'Could not import ' .. clip.path)
                    imported[clip.path] = media[1]
                end
            end
        end
        local timeline = require_value(pool:CreateEmptyTimeline(manifest.timeline_name or 'Orka First pass'),
                                       'Could not create the timeline')
        require_value(timeline:SetStartTimecode('00:00:00:00'), 'Could not set zero timeline start')
        require_value(project:SetCurrentTimeline(timeline), 'Could not activate the timeline')
        timeline:SetTrackName('video', 1, 'Story')
        local max_track = 1
        for _, clip in ipairs(manifest.audio) do max_track = math.max(max_track, clip.track) end
        while timeline:GetTrackCount('audio') < max_track do
            require_value(timeline:AddTrack('audio', 'stereo'), 'Could not add narration track')
        end
        for index = 1, max_track do
            timeline:SetTrackName('audio', index, index == 1 and 'Narration' or 'Audio ' .. index)
        end
        for _, scene in ipairs(manifest.scenes) do
            append_clip(pool, timeline, imported[scene.path], scene, 1, 1)
            timeline:AddMarker(scene.record_frame, 'Blue', scene.title or scene.id,
                               scene.note or '', scene.frames, scene.id)
        end
        for _, clip in ipairs(manifest.audio) do
            append_clip(pool, timeline, imported[clip.path], clip, 2, clip.track)
        end
        report.timeline_frames = timeline:GetEndFrame() - timeline:GetStartFrame()
        require_value(report.timeline_frames == manifest.total_frames, 'Timeline duration mismatch')
        report.video_items = #(timeline:GetItemListInTrack('video', 1) or {})
        report.audio_items = 0
        for index = 1, max_track do
            report.audio_items = report.audio_items + #(timeline:GetItemListInTrack('audio', index) or {})
        end
        app:OpenPage('deliver')
        local formats = project:GetRenderFormats()
        local codecs = project:GetRenderCodecs('mp4')
        report.render_formats = formats
        report.mp4_codecs = codecs
        local mp4_available = false
        for _, format in pairs(formats or {}) do if format == 'mp4' then mp4_available = true end end
        require_value(mp4_available, 'MP4 rendering is unavailable')
        local codec
        for label, value in pairs(codecs or {}) do
            if label:lower():gsub('[^a-z0-9]', ''):find('h264') then codec = value; break end
        end
        require_value(codec, 'H.264 rendering is unavailable')
        require_value(project:SetCurrentRenderMode(1), 'Could not select single-clip rendering')
        require_value(project:SetCurrentRenderFormatAndCodec('mp4', codec), 'Could not select H.264 MP4')
        local settings = {
            SelectAllFrames = true, TargetDir = manifest.output_dir, CustomName = manifest.output_name,
            FormatWidth = 1920, FormatHeight = 1080, FrameRate = 30,
            ExportVideo = true, ExportAudio = true, AudioCodec = 'aac',
        }
        require_value(project:SetRenderSettings(settings), 'Resolve rejected the render settings')
        local job = require_value(project:AddRenderJob(), 'Could not queue the render')
        report.render_job_id = job
        report.render_settings = settings
        require_value(manager:SaveProject(), 'Could not save the new project')
        report.project_export = base .. '.drp'
        require_value(manager:ExportProject(project_name, report.project_export), 'Could not export the project')
        report.status = 'queued'
        write_report(report_path, report)
        require_value(project:StartRendering({job}), 'Could not start rendering')
        report.status = 'rendering'
        write_report(report_path, report)
        print(string.format('Resolve is rendering %s (%.2fs). Project: %s',
                            manifest.output_name, manifest.duration_seconds, project_name))
        print('Report: ' .. report_path)
        if not io then
            print('ORKA_REPORT:' .. json({status = report.status, project_name = project_name,
                resolve_version = report.resolve_version, render_job_id = job,
                timeline_frames = report.timeline_frames, video_items = report.video_items,
                audio_items = report.audio_items, project_export = report.project_export}))
        end
    end)
    if not ok then
        report.status = 'failed'
        report.error = tostring(problem)
        write_report(report_path, report)
        error(problem)
    end
    return report
end

local function status(report)
    local app = require_value(resolve or bmd.scriptapp('Resolve'), 'Resolve is not available')
    local project = app:GetProjectManager():GetCurrentProject()
    require_value(project and project:GetName() == report.project_name, 'This assembly is not the active project')
    report.render_status = project:GetRenderJobStatus(report.render_job_id)
    report.status = project:IsRenderingInProgress() and 'rendering' or 'render-stopped'
    write_report(report.manifest.output_dir .. '/' .. report.manifest.output_name .. '.report.json', report)
    print(json(report.render_status))
    return report.render_status
end

return {assemble = assemble, status = status}
