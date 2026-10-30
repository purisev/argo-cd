if obj.metadata.annotations == nil then
  obj.metadata.annotations = {}
end
obj.metadata.annotations["serving.kserve.io/stop"] = "true"
return obj
