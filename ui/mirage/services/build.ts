import { ListBuildsRequest, ListBuildsResponse, GetBuildRequest } from 'waypoint-pb';
import { Request, Response } from 'miragejs';
import { decode } from '../helpers/protobufs';

export function list(schema: any, { requestBody }: Request): Response {
  let requestMsg = decode(ListBuildsRequest, requestBody);
  let projectName = requestMsg.getApplication().getProject();
  let appName = requestMsg.getApplication().getApplication();
  let workspaceName = requestMsg.getWorkspace().getWorkspace();
  let project = schema.projects.findBy({ name: projectName });
  let application = schema.applications.findBy({ name: appName, projectId: project.id });
  let workspace = schema.workspaces.findBy({ name: workspaceName });
  let builds = schema.builds.where({ applicationId: application?.id, workspaceId: workspace?.id });
  let buildProtobufs = builds.models.map((b) => b.toProtobuf());
  let resp = new ListBuildsResponse();

  resp.setBuildsList(buildProtobufs);

  return this.serialize(resp, 'application');
}

export function get(schema: any, { requestBody }: Request): Response {
  let requestMsg = decode(GetBuildRequest, requestBody);
  let id = requestMsg.getRef().getId();
  let model = schema.builds.find(id);
  let build = model?.toProtobuf();

  return this.serialize(build, 'application');
}
