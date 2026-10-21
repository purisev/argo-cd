if obj.metadata.annotations == nil then
    obj.metadata.annotations = {}
end

local key = "argocd.argoproj.io/suspend"
if obj.metadata.annotations[key] == "true" then
    obj.metadata.annotations[key] = nil
else
    obj.metadata.annotations[key] = "true"
end

return obj
